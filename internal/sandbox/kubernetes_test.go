package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tf-agent/tf-agent/internal/sandbox"
)

// These tests exercise K8sJobExecutor against a REAL Kubernetes cluster —
// there is no fake/mock client-go transport involved. They are gated behind
// requireK8s so `go test ./...` stays green in any environment without
// cluster access, matching this repo's existing Docker/NATS-gated test
// pattern (internal/sandbox/docker_test.go, internal/queue/nats_test.go).
//
// Override TF_AGENT_K8S_TEST_NAMESPACE / TF_AGENT_K8S_TEST_IMAGE to point at
// a different pre-existing namespace/image; defaults match this repo's own
// verification run (see docs/SANDBOX.md) against a kind cluster where the
// real iac-agent-sandbox image couldn't be loaded onto the (amd64) nodes from
// this (arm64) dev machine, so a small already-pullable stand-in image is
// used instead — K8sJobExecutor's mechanics don't depend on which image runs.
//
// IMPORTANT: these tests never create or delete the target namespace itself
// (K8sJobExecutor's own contract — see its doc comment) — they skip if it
// doesn't already exist, and only ever touch Jobs/Pods/NetworkPolicies
// inside it.
const (
	defaultTestNamespace = "iac-agent-sandbox"
	defaultTestImage     = "busybox:latest"
)

func k8sTestNamespace() string {
	if v := os.Getenv("TF_AGENT_K8S_TEST_NAMESPACE"); v != "" {
		return v
	}
	return defaultTestNamespace
}

func k8sTestImage() string {
	if v := os.Getenv("TF_AGENT_K8S_TEST_IMAGE"); v != "" {
		return v
	}
	return defaultTestImage
}

// requireK8s skips the test unless kubectl is installed, the ambient
// kubeconfig can reach a real API server, and the target namespace already
// exists — mirroring requireDocker's "the real dependency actually works,
// not just that a binary exists" spirit.
func requireK8s(t *testing.T) *sandbox.K8sJobExecutor {
	t.Helper()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not installed — skipping kubernetes sandbox integration test")
	}

	jobExecutor, err := sandbox.NewK8sJobExecutor("", k8sTestNamespace(), k8sTestImage(), "", "")
	if err != nil {
		t.Skipf("could not build K8sJobExecutor (no reachable kubeconfig?): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := jobExecutor.Clientset.CoreV1().Namespaces().Get(ctx, k8sTestNamespace(), metav1.GetOptions{}); err != nil {
		t.Skipf("namespace %q not reachable (cluster unreachable, or namespace doesn't exist — this package never creates it): %v", k8sTestNamespace(), err)
	}
	return jobExecutor
}

// countSandboxPods returns the number of Jobs and Pods left in ns carrying
// K8sJobExecutor's own "app=iac-agent-sandbox" label — used to assert no
// leftover test debris, the Kubernetes equivalent of docker_test.go's
// `docker ps` leak check.
func countSandboxPods(t *testing.T, e *sandbox.K8sJobExecutor, ns string) (jobs, pods int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	jobList, err := e.Clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=iac-agent-sandbox"})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	podList, err := e.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=iac-agent-sandbox"})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	return len(jobList.Items), len(podList.Items)
}

// TestK8sJobExecutor_RealCommandAndUser proves the whole plumbing works end
// to end against a real cluster: Job creation, waiting for the pod to
// become Running, and a real exec'd command returning real output — and
// that the pod actually runs as the configured non-root uid, not root.
func TestK8sJobExecutor_RealCommandAndUser(t *testing.T) {
	e := requireK8s(t)
	ns := k8sTestNamespace()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stdout, stderr, err := e.Run(ctx, dir, "id", "-u")
	if err != nil {
		t.Fatalf("Run(id -u) failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	got := strings.TrimSpace(stdout)
	want := strconv.FormatInt(sandbox.DefaultKubeRunAsUser, 10)
	if got != want {
		t.Errorf("id -u = %q, want %q (DefaultKubeRunAsUser) — pod did not run as the configured non-root uid", got, want)
	}

	jobs, pods := countSandboxPods(t, e, ns)
	if jobs != 0 || pods != 0 {
		t.Errorf("leftover sandbox objects after a successful Run: %d jobs, %d pods", jobs, pods)
	}
}

// TestK8sJobExecutor_RealFileStaging proves the exec/tar file-staging
// mechanism actually transfers real file *content*, not just that some
// command runs — this is the core design question the brief asked to
// prototype against a real cluster before committing to it over the
// PVC/object-storage alternatives.
func TestK8sJobExecutor_RealFileStaging(t *testing.T) {
	e := requireK8s(t)

	dir := t.TempDir()
	const marker = "hello-from-tf-agent-batchC-79f3"
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// A subdirectory too, to prove the tar walk preserves structure, not
	// just top-level files.
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir fixture subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subdir", "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatalf("write nested fixture: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stdout, stderr, err := e.Run(ctx, dir, "sh", "-c", "cat marker.txt && cat subdir/nested.txt")
	if err != nil {
		t.Fatalf("Run failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stdout, marker) {
		t.Errorf("staged file content missing from output; got stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "nested") {
		t.Errorf("staged nested-directory file content missing from output; got stdout:\n%s", stdout)
	}
}

// TestK8sJobExecutor_CancelCleansUpNoLeak proves that cancelling ctx during
// a long-running command actually deletes the Job/Pod — not just that Run
// returns — the Kubernetes equivalent of
// TestDockerExecutor_CancelKillsContainerNoLeak.
func TestK8sJobExecutor_CancelCleansUpNoLeak(t *testing.T) {
	e := requireK8s(t)
	ns := k8sTestNamespace()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := e.Run(ctx, dir, "sh", "-c", "sleep 300")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Run to return an error for a cancelled/timed-out command")
	}
	// Bounded generously: Job creation, pod scheduling/start, and the 3s
	// command timeout itself all count against this, plus this Run's own
	// cleanup (bounded internally to 30s) happens before Run returns.
	if elapsed > 45*time.Second {
		t.Fatalf("Run took %s to return after a 3s context timeout — cleanup is not prompt", elapsed)
	}

	jobs, pods := countSandboxPods(t, e, ns)
	if jobs != 0 || pods != 0 {
		t.Fatalf("leaked sandbox objects after cancellation: %d jobs, %d pods", jobs, pods)
	}
}

// TestK8sJobExecutor_NetworkPolicyStructure asserts the NetworkPolicy
// K8sJobExecutor ensures exists has the shape it's supposed to (selects
// every pod in the namespace, denies all egress) — a stable, cluster-CNI-
// independent correctness check. Whether the CNI actually *enforces* it is
// a separate, cluster-specific question answered manually and documented in
// docs/SANDBOX.md (this cluster's CNI, kindnetd, does enforce it — confirmed
// via a real A/B egress probe).
func TestK8sJobExecutor_NetworkPolicyStructure(t *testing.T) {
	e := requireK8s(t)
	ns := k8sTestNamespace()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if _, _, err := e.Run(ctx, dir, "true"); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	getCtx, getCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer getCancel()
	np, err := e.Clientset.NetworkingV1().NetworkPolicies(ns).Get(getCtx, "iac-agent-sandbox-deny-egress", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected deny-egress NetworkPolicy to exist: %v", err)
	}
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("expected an empty PodSelector (all pods in namespace), got %+v", np.Spec.PodSelector)
	}
	foundEgress := false
	for _, pt := range np.Spec.PolicyTypes {
		if pt == "Egress" {
			foundEgress = true
		}
	}
	if !foundEgress {
		t.Errorf("expected PolicyTypes to include Egress, got %+v", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 0 {
		t.Errorf("expected zero Egress rules (deny-all), got %d", len(np.Spec.Egress))
	}
}
