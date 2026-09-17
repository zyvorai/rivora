// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

// addressPoolGVR matches api/v1alpha1's AddressPool CRD
// (rivora.zyvor.dev/v1alpha1, plural "addresspools").
var addressPoolGVR = schema.GroupVersionResource{Group: "rivora.zyvor.dev", Version: "v1alpha1", Resource: "addresspools"}

// WorkloadStatus is the rollout-status verdict for one Deployment or
// DaemonSet — kept independent of how the underlying object was fetched so
// it's unit-testable against hand-built fixtures (see status_test.go).
type WorkloadStatus struct {
	Name      string
	Desired   int32
	Current   int32
	Ready     int32
	Available int32
	Degraded  []string // e.g. "rivorad-abc12: CrashLoopBackOff"
}

// Healthy reports whether every desired replica is ready and available,
// and none are in a degraded state.
func (s WorkloadStatus) Healthy() bool {
	return len(s.Degraded) == 0 && s.Desired > 0 && s.Ready == s.Desired && s.Available == s.Desired
}

// ClusterStatus is `rivora status`'s full report.
type ClusterStatus struct {
	ReleaseName    string
	ChartVersion   string
	AppVersion     string
	Rivorad        WorkloadStatus
	Controller     WorkloadStatus
	AddressPoolErr error // non-nil if the AddressPool CRD isn't installed/reachable
}

// DaemonSetStatus derives a WorkloadStatus from a live DaemonSet.
func DaemonSetStatus(ds *appsv1.DaemonSet, pods []corev1.Pod) WorkloadStatus {
	return WorkloadStatus{
		Name:      ds.Name,
		Desired:   ds.Status.DesiredNumberScheduled,
		Current:   ds.Status.CurrentNumberScheduled,
		Ready:     ds.Status.NumberReady,
		Available: ds.Status.NumberAvailable,
		Degraded:  degradedPods(pods),
	}
}

// DeploymentStatus derives a WorkloadStatus from a live Deployment.
func DeploymentStatus(dep *appsv1.Deployment, pods []corev1.Pod) WorkloadStatus {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	return WorkloadStatus{
		Name:      dep.Name,
		Desired:   desired,
		Current:   dep.Status.Replicas,
		Ready:     dep.Status.ReadyReplicas,
		Available: dep.Status.AvailableReplicas,
		Degraded:  degradedPods(pods),
	}
}

func degradedPods(pods []corev1.Pod) []string {
	var degraded []string
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && isDegradedReason(cs.State.Waiting.Reason) {
				degraded = append(degraded, fmt.Sprintf("%s: %s", p.Name, cs.State.Waiting.Reason))
			}
		}
	}
	return degraded
}

func isDegradedReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError":
		return true
	default:
		return false
	}
}

// GetClusterStatus queries the cluster for rivorad/rivora-controller
// rollout status plus the Helm release's version info.
func GetClusterStatus(cluster ClusterOptions, releaseName string) (*ClusterStatus, error) {
	clients, err := cluster.Clients()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	cs := clients.Clientset

	rivoradStatus, err := fetchDaemonSetStatus(ctx, cs, cluster.Namespace, releaseName+"-rivorad")
	if err != nil {
		return nil, fmt.Errorf("get rivorad DaemonSet: %w", err)
	}
	controllerStatus, err := fetchDeploymentStatus(ctx, cs, cluster.Namespace, releaseName+"-controller")
	if err != nil {
		return nil, fmt.Errorf("get rivora-controller Deployment: %w", err)
	}

	status := &ClusterStatus{
		ReleaseName: releaseName,
		Rivorad:     *rivoradStatus,
		Controller:  *controllerStatus,
	}

	rel, err := GetRelease(cluster, releaseName)
	if err == nil && rel != nil && rel.Chart != nil && rel.Chart.Metadata != nil {
		status.ChartVersion = rel.Chart.Metadata.Version
		status.AppVersion = rel.Chart.Metadata.AppVersion
	}

	if _, err := clients.Dynamic.Resource(addressPoolGVR).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		status.AddressPoolErr = err
	}

	return status, nil
}

func fetchDaemonSetStatus(ctx context.Context, cs kubernetes.Interface, namespace, name string) (*WorkloadStatus, error) {
	ds, err := cs.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := podsForSelector(ctx, cs, namespace, ds.Spec.Selector)
	if err != nil {
		return nil, err
	}
	st := DaemonSetStatus(ds, pods)
	return &st, nil
}

func fetchDeploymentStatus(ctx context.Context, cs kubernetes.Interface, namespace, name string) (*WorkloadStatus, error) {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pods, err := podsForSelector(ctx, cs, namespace, dep.Spec.Selector)
	if err != nil {
		return nil, err
	}
	st := DeploymentStatus(dep, pods)
	return &st, nil
}

func podsForSelector(ctx context.Context, cs kubernetes.Interface, namespace string, selector *metav1.LabelSelector) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
