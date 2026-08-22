package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

// Default settings applied to a K8sJobExecutor constructed without explicit
// overrides. Memory/CPUs use Kubernetes resource.Quantity syntax (e.g.
// "512Mi", "1") — deliberately not shared with DockerExecutor's
// DefaultMemory/DefaultCPUs constants, which use Docker's own
// --memory/--cpus syntax ("512m", "1"); the two are not always
// interchangeable strings (Kubernetes parses a bare "m" suffix as milli,
// not megabytes).
const (
	DefaultKubeNamespace = "iac-agent-sandbox"
	DefaultKubeMemory    = "512Mi"
	DefaultKubeCPUs      = "1"

	// DefaultKubeRunAsUser matches the non-root uid baked into
	// docker/sandbox/Dockerfile (USER sandbox, uid 10001) — K8sJobExecutor
	// runs the exact same sandbox image DockerExecutor does, just scheduled
	// as a Kubernetes Job instead of a local `docker run`.
	DefaultKubeRunAsUser = 10001

	// stagingCommandTimeoutSeconds bounds how long the Job's pod stays
	// alive on its own. Run always deletes the Job explicitly on every
	// exit path (success, command failure, or ctx cancellation) — this is
	// only a safety-net ceiling in case that explicit delete somehow never
	// runs (e.g. the process running Run is itself killed -9 before its
	// deferred cleanup fires). Mirrors DockerExecutor's --rm being the
	// intended cleanup and `docker rm -f` being the backstop: this is that
	// backstop's own backstop.
	stagingCommandTimeoutSeconds = 3600

	// networkPolicyName is the deny-all-egress NetworkPolicy K8sJobExecutor
	// ensures exists in its namespace — the closest cluster-level
	// equivalent to DockerExecutor's --network=none. See docs/sandbox.md
	// for what was verified about this cluster's CNI actually enforcing it.
	networkPolicyName = "iac-agent-sandbox-deny-egress"

	sandboxContainerName = "sandbox"
	workspaceMountPath   = "/workspace"
)

// K8sJobExecutor implements Executor by running each command inside a fresh,
// locked-down Kubernetes Job — the multi-node production analogue of
// DockerExecutor's local `docker run`. It assumes Namespace already exists
// (created out-of-band, e.g. by cluster ops) and that its ServiceAccount has
// RBAC to create/get/list/delete Jobs, Pods, Pods/exec, and (if
// EnsureNetworkPolicy is set) NetworkPolicies within that one namespace —
// deliberately not cluster-wide.
//
// Containment properties, mirroring DockerExecutor where Kubernetes has a
// direct equivalent (see docs/sandbox.md for what was actually verified
// against a real cluster, including the NetworkPolicy enforcement caveat):
//   - PodSecurityContext/SecurityContext RunAsNonRoot + RunAsUser: no root
//     inside the container, the same posture as DockerExecutor never
//     passing --user (relying on the sandbox image's own non-root USER).
//   - SecurityContext.Capabilities.Drop=["ALL"] +
//     AllowPrivilegeEscalation=false: the Kubernetes equivalent of
//     --cap-drop=ALL --security-opt=no-new-privileges.
//   - Resources.Requests/Limits: the equivalent of --memory/--cpus.
//   - A deny-all-egress NetworkPolicy in the namespace: the closest
//     cluster-level equivalent to --network=none. This is only a real
//     containment property if the cluster's CNI enforces NetworkPolicy —
//     confirm this for your cluster rather than assuming either way. This
//     project's own kind test cluster's kindnetd build does enforce it,
//     confirmed via a real A/B egress probe (see docs/sandbox.md, verified
//     for real 2026-08-22).
//   - The Job (and therefore its Pod) is always deleted on the way out,
//     regardless of how Run exits — the equivalent of DockerExecutor's
//     --rm + `docker rm -f` backstop, just at cluster scope.
//
// File staging: rather than the PVC/object-storage round trip
// docs/sandbox.md originally sketched, Run stages workDir into the running
// pod by streaming a tar archive over the pod exec API (the same mechanism
// `kubectl cp` uses under the hood via client-go's remotecommand package) —
// this needs no extra cluster infrastructure (no PVC, no object storage
// round trip) and was verified working against a real cluster. See
// docs/sandbox.md for the full writeup of why this was chosen over the
// PVC/object-storage alternatives.
//
// The actual command (name/args) is also run via pod exec rather than as
// the Job's own container command, so its stdout/stderr are captured
// directly from the exec stream — separately, matching the Executor
// contract exactly — instead of needing to parse the pod's combined log
// output.
type K8sJobExecutor struct {
	// Clientset and RESTConfig are both required: Clientset for the
	// typed Job/Pod/NetworkPolicy API calls, RESTConfig because
	// remotecommand's SPDY executor needs the raw *rest.Config to
	// upgrade the exec subresource connection.
	Clientset  kubernetes.Interface
	RESTConfig *rest.Config

	Namespace string
	Image     string
	Memory    string // Kubernetes resource.Quantity string, e.g. "512Mi"
	CPUs      string // Kubernetes resource.Quantity string, e.g. "1"
	RunAsUser int64

	// EnsureNetworkPolicy controls whether Run creates (idempotently) a
	// deny-all-egress NetworkPolicy in Namespace before running the Job.
	// Defaults to true via NewK8sJobExecutor.
	EnsureNetworkPolicy bool
}

// NewK8sJobExecutor builds a K8sJobExecutor from a kubeconfig path (empty
// uses the standard client-go loading rules: $KUBECONFIG, then
// ~/.kube/config, then in-cluster config) and fills in sane defaults for any
// empty field.
func NewK8sJobExecutor(kubeconfigPath, namespace, image, memory, cpus string) (*K8sJobExecutor, error) {
	cfg, err := loadKubeConfig(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("sandbox: load kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("sandbox: build clientset: %w", err)
	}
	if namespace == "" {
		namespace = DefaultKubeNamespace
	}
	if image == "" {
		image = DefaultImage
	}
	if memory == "" {
		memory = DefaultKubeMemory
	}
	if cpus == "" {
		cpus = DefaultKubeCPUs
	}
	return &K8sJobExecutor{
		Clientset:           clientset,
		RESTConfig:          cfg,
		Namespace:           namespace,
		Image:               image,
		Memory:              memory,
		CPUs:                cpus,
		RunAsUser:           DefaultKubeRunAsUser,
		EnsureNetworkPolicy: true,
	}, nil
}

// loadKubeConfig loads a *rest.Config either from an explicit kubeconfig
// path or, when path is empty, from the standard client-go resolution order
// (in-cluster config when running inside a pod, else $KUBECONFIG, else
// ~/.kube/config).
func loadKubeConfig(path string) (*rest.Config, error) {
	if path == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		loadingRules.ExplicitPath = path
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// Run executes name+args inside a fresh Kubernetes Job's pod, with workDir
// staged in at /workspace via exec/tar. See the K8sJobExecutor doc comment
// for the full containment/cleanup contract.
func (k *K8sJobExecutor) Run(ctx context.Context, workDir string, name string, args ...string) (string, string, error) {
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("sandbox: resolve workDir: %w", err)
	}

	jobName, err := randomJobName()
	if err != nil {
		return "", "", fmt.Errorf("sandbox: generate job name: %w", err)
	}

	if k.EnsureNetworkPolicy {
		if err := k.ensureDenyEgressNetworkPolicy(ctx); err != nil {
			return "", "", fmt.Errorf("sandbox: ensure NetworkPolicy: %w", err)
		}
	}

	job, err := k.buildJob(jobName)
	if err != nil {
		return "", "", fmt.Errorf("sandbox: build job spec: %w", err)
	}

	if _, err := k.Clientset.BatchV1().Jobs(k.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", "", fmt.Errorf("sandbox: create job: %w", err)
	}

	// Always clean up the Job (and therefore its Pod) on the way out,
	// regardless of how this function returns — the Kubernetes equivalent
	// of DockerExecutor's --rm + `docker rm -f` backstop. A fresh,
	// short-lived context is used so cleanup still happens even when ctx
	// itself is already cancelled or expired (e.g. this Run is returning
	// BECAUSE ctx was cancelled).
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		k.deleteJobAndWait(cleanupCtx, jobName)
	}()

	podName, err := k.waitForPodRunning(ctx, jobName)
	if err != nil {
		return "", "", fmt.Errorf("sandbox: wait for pod running: %w", err)
	}

	// Stage workDir into the pod by streaming a tar archive over exec — the
	// same mechanism `kubectl cp` uses under the hood.
	var stageStdout, stageStderr bytes.Buffer
	if err := k.execTarIn(ctx, podName, absWorkDir, &stageStdout, &stageStderr); err != nil {
		return stageStdout.String(), stageStderr.String(), fmt.Errorf("sandbox: stage workDir: %w (stderr: %s)", err, stageStderr.String())
	}

	var stdout, stderr bytes.Buffer
	runErr := k.execCommand(ctx, podName, name, args, &stdout, &stderr)

	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), stderr.String(), fmt.Errorf("sandbox: command aborted (%v): %w", ctxErr, runErr)
		}
		return stdout.String(), stderr.String(), runErr
	}
	return stdout.String(), stderr.String(), nil
}

// buildJob constructs the Job spec: a single pod, single container, running
// a long-enough-lived placeholder command (see stagingCommandTimeoutSeconds)
// so Run can exec into it twice — once to stage workDir, once to run the
// real command — before deleting the whole Job.
func (k *K8sJobExecutor) buildJob(jobName string) (*batchv1.Job, error) {
	memQty, err := resource.ParseQuantity(k.Memory)
	if err != nil {
		return nil, fmt.Errorf("parse memory %q: %w", k.Memory, err)
	}
	cpuQty, err := resource.ParseQuantity(k.CPUs)
	if err != nil {
		return nil, fmt.Errorf("parse cpus %q: %w", k.CPUs, err)
	}

	backoffLimit := int32(0)
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	runAsUser := k.RunAsUser

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: k.Namespace,
			Labels:    map[string]string{"app": "iac-agent-sandbox"},
		},
		Spec: batchv1.JobSpec{
			// No retries: a failed pod should surface as a failed Run, not
			// silently retry with new (and differently-staged) workDir
			// contents.
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "iac-agent-sandbox"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: boolPtr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
						// FSGroup makes the emptyDir /workspace volume
						// group-writable by RunAsUser — otherwise a
						// non-root container can't write into it.
						FSGroup: &runAsUser,
					},
					Containers: []corev1.Container{
						{
							Name:       sandboxContainerName,
							Image:      k.Image,
							Command:    []string{"sh", "-c", fmt.Sprintf("sleep %d", stagingCommandTimeoutSeconds)},
							WorkingDir: workspaceMountPath,
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot:             &runAsNonRoot,
								RunAsUser:                &runAsUser,
								AllowPrivilegeEscalation: &allowPrivilegeEscalation,
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: memQty,
									corev1.ResourceCPU:    cpuQty,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: memQty,
									corev1.ResourceCPU:    cpuQty,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: workspaceMountPath},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name:         "workspace",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}, nil
}

// waitForPodRunning polls for the Job's pod to reach the Running phase,
// bounded by ctx.
func (k *K8sJobExecutor) waitForPodRunning(ctx context.Context, jobName string) (string, error) {
	selector := "job-name=" + jobName
	for {
		pods, err := k.Clientset.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return "", fmt.Errorf("list pods: %w", err)
		}
		for _, p := range pods.Items {
			switch p.Status.Phase {
			case corev1.PodRunning:
				return p.Name, nil
			case corev1.PodFailed:
				return "", fmt.Errorf("pod %s failed before becoming ready: %s", p.Name, p.Status.Reason)
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// execTarIn streams workDir's contents into the pod as a tar archive over
// exec, extracting into workspaceMountPath. This is the file-staging
// mechanism in place of a PVC or object-storage round trip — see the
// K8sJobExecutor doc comment.
func (k *K8sJobExecutor) execTarIn(ctx context.Context, podName, workDir string, stdout, stderr io.Writer) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(tarDirInto(pw, workDir))
	}()

	cmd := []string{"sh", "-c", "mkdir -p " + workspaceMountPath + " && tar -xf - -C " + workspaceMountPath}
	return k.execStream(ctx, podName, cmd, pr, stdout, stderr)
}

// tarDirInto writes dir's contents (relative paths, no leading dir
// component) as a tar stream to w.
func tarDirInto(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() || !info.Mode().IsRegular() {
			// Skip non-regular files (symlinks, devices, sockets) — not
			// expected in a task workDir, and not safe to blindly stream.
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// execCommand runs name+args inside the pod via exec, with a `cd
// workspaceMountPath && exec "$0" "$@"` wrapper so the command's working
// directory matches the role workDir plays for the host-exec and Docker
// paths — without ever shell-interpolating name/args (they're passed as
// positional parameters, the same injection-safety property
// DockerExecutor's exec.CommandContext gets for free).
func (k *K8sJobExecutor) execCommand(ctx context.Context, podName, name string, args []string, stdout, stderr io.Writer) error {
	script := "cd " + workspaceMountPath + ` && exec "$0" "$@"`
	cmd := append([]string{"sh", "-c", script, name}, args...)
	return k.execStream(ctx, podName, cmd, nil, stdout, stderr)
}

// execStream is the shared remotecommand plumbing for both execTarIn and
// execCommand: it opens a pod exec subresource connection over SPDY and
// streams stdin/stdout/stderr until the command completes or ctx is
// cancelled.
func (k *K8sJobExecutor) execStream(ctx context.Context, podName string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := k.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(k.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: sandboxContainerName,
			Command:   command,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(k.RESTConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build exec stream: %w", err)
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// ensureDenyEgressNetworkPolicy idempotently creates a NetworkPolicy
// selecting every pod in the namespace and denying all egress — the closest
// cluster-level equivalent to DockerExecutor's --network=none. This is a
// real containment property only if the cluster's CNI enforces
// NetworkPolicy; see docs/sandbox.md for what was verified for the kind
// cluster this was built against (its kindnetd build does enforce it,
// confirmed via a real A/B egress probe).
func (k *K8sJobExecutor) ensureDenyEgressNetworkPolicy(ctx context.Context) error {
	npClient := k.Clientset.NetworkingV1().NetworkPolicies(k.Namespace)
	if _, err := npClient.Get(ctx, networkPolicyName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName,
			Namespace: k.Namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // empty selector = every pod in the namespace
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			// No Egress rules => deny all egress from selected pods.
		},
	}
	_, err := npClient.Create(ctx, np, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return nil // race with a concurrent Run: already created, fine.
	}
	return err
}

// deleteJobAndWait deletes the Job, force-deletes any pods it left behind as
// a backstop (mirroring DockerExecutor's unconditional `docker rm -f` after
// `docker run --rm`), and polls — bounded by ctx — until no matching pods
// remain. Errors are intentionally swallowed: this is a best-effort cleanup
// backstop, not something Run should fail on, matching DockerExecutor's own
// `_ = exec.CommandContext(...).Run()` cleanup pattern.
func (k *K8sJobExecutor) deleteJobAndWait(ctx context.Context, jobName string) {
	propagation := metav1.DeletePropagationForeground
	_ = k.Clientset.BatchV1().Jobs(k.Namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})

	gracePeriod := int64(0)
	selector := "job-name=" + jobName
	_ = k.Clientset.CoreV1().Pods(k.Namespace).DeleteCollection(ctx,
		metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod},
		metav1.ListOptions{LabelSelector: selector},
	)

	for {
		pods, err := k.Clientset.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil || len(pods.Items) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func randomJobName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "iac-agent-sandbox-" + hex.EncodeToString(b), nil
}

func boolPtr(b bool) *bool { return &b }
