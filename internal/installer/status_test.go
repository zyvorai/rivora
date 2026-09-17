// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package installer

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDaemonSetStatusHealthy(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rivora-rivorad"},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 3,
			CurrentNumberScheduled: 3,
			NumberReady:            3,
			NumberAvailable:        3,
		},
	}
	st := DaemonSetStatus(ds, nil)
	if !st.Healthy() {
		t.Errorf("expected healthy, got %+v", st)
	}
}

func TestDaemonSetStatusDegradedByReadyCount(t *testing.T) {
	ds := &appsv1.DaemonSet{
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberReady: 2, NumberAvailable: 2},
	}
	st := DaemonSetStatus(ds, nil)
	if st.Healthy() {
		t.Errorf("2/3 ready should not be healthy, got %+v", st)
	}
}

func TestDaemonSetStatusDegradedByPodReason(t *testing.T) {
	ds := &appsv1.DaemonSet{
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1, NumberAvailable: 1},
	}
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "rivora-rivorad-abc12"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}}
	st := DaemonSetStatus(ds, pods)
	if st.Healthy() {
		t.Errorf("a CrashLoopBackOff pod should mark the workload degraded even if counts look fine, got %+v", st)
	}
	if len(st.Degraded) != 1 || st.Degraded[0] != "rivora-rivorad-abc12: CrashLoopBackOff" {
		t.Errorf("Degraded = %v", st.Degraded)
	}
}

func TestDeploymentStatusDefaultsReplicasToOne(t *testing.T) {
	dep := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{}, // Replicas nil — API server default is 1
		Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	st := DeploymentStatus(dep, nil)
	if st.Desired != 1 {
		t.Errorf("Desired = %d, want 1 (nil Spec.Replicas should default to 1, matching API server behavior)", st.Desired)
	}
	if !st.Healthy() {
		t.Errorf("expected healthy, got %+v", st)
	}
}

func TestDeploymentStatusRespectsExplicitReplicas(t *testing.T) {
	two := int32(2)
	dep := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	st := DeploymentStatus(dep, nil)
	if st.Healthy() {
		t.Errorf("1/2 ready should not be healthy, got %+v", st)
	}
}

func TestIgnoresNonDegradedWaitingReasons(t *testing.T) {
	pods := []corev1.Pod{{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}},
		},
	}}
	if got := degradedPods(pods); len(got) != 0 {
		t.Errorf("ContainerCreating is a normal transient state, not degraded: got %v", got)
	}
}
