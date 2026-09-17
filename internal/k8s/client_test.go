// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://example.invalid:6443
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: fake-token
`

// disableInCluster makes rest.InClusterConfig() fail deterministically,
// regardless of whether the test happens to run inside a real Pod, so
// BuildConfig's kubeconfig-fallback branches are actually exercised.
func disableInCluster(t *testing.T) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func writeKubeconfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(fakeKubeconfig), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestBuildConfigExplicitPathWins(t *testing.T) {
	disableInCluster(t)
	path := writeKubeconfig(t, t.TempDir())

	cfg, err := BuildConfig(path)
	if err != nil {
		t.Fatalf("BuildConfig(%q): %v", path, err)
	}
	if cfg.Host != "https://example.invalid:6443" {
		t.Errorf("cfg.Host = %q, want the server from the explicit kubeconfig", cfg.Host)
	}
}

func TestBuildConfigFallsBackToKUBECONFIGEnvVar(t *testing.T) {
	disableInCluster(t)
	path := writeKubeconfig(t, t.TempDir())
	t.Setenv("KUBECONFIG", path)

	cfg, err := BuildConfig("")
	if err != nil {
		t.Fatalf("BuildConfig(\"\") with $KUBECONFIG set: %v", err)
	}
	if cfg.Host != "https://example.invalid:6443" {
		t.Errorf("cfg.Host = %q, want the server from $KUBECONFIG", cfg.Host)
	}
}

func TestBuildConfigFallsBackToHomeDir(t *testing.T) {
	disableInCluster(t)
	t.Setenv("KUBECONFIG", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".kube"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeKubeconfig(t, filepath.Join(home, ".kube"))

	cfg, err := BuildConfig("")
	if err != nil {
		t.Fatalf("BuildConfig(\"\") with only ~/.kube/config present: %v", err)
	}
	if cfg.Host != "https://example.invalid:6443" {
		t.Errorf("cfg.Host = %q, want the server from ~/.kube/config", cfg.Host)
	}
}

func TestBuildConfigNoneAvailable(t *testing.T) {
	disableInCluster(t)
	t.Setenv("KUBECONFIG", "")
	// An empty, real HOME with no .kube/config: clientcmd.BuildConfigFromFlags
	// is handed a path that doesn't exist, so this exercises the same
	// no-config error path as a genuinely empty $HOME would.
	t.Setenv("HOME", t.TempDir())

	_, err := BuildConfig("")
	if err == nil {
		t.Fatal("BuildConfig(\"\") with no in-cluster config, no $KUBECONFIG and no ~/.kube/config should error")
	}
}

func TestNewBuildsClientsFromConfig(t *testing.T) {
	disableInCluster(t)
	path := writeKubeconfig(t, t.TempDir())
	cfg, err := BuildConfig(path)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	clients, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if clients.Clientset == nil {
		t.Error("Clients.Clientset is nil")
	}
	if clients.Dynamic == nil {
		t.Error("Clients.Dynamic is nil")
	}
	if clients.Config != cfg {
		t.Error("Clients.Config should be the same *rest.Config passed in")
	}
}

func TestBuildConfigErrorMentionsBothFallbacks(t *testing.T) {
	disableInCluster(t)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", "") // os.UserHomeDir() fails on an empty $HOME

	_, err := BuildConfig("")
	if err == nil {
		t.Fatal("expected an error with no in-cluster config, no $KUBECONFIG, and no resolvable home dir")
	}
	if !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("error %q should mention kubeconfig, to point the operator at --kubeconfig/$KUBECONFIG", err.Error())
	}
}
