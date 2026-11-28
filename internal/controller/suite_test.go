package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"llm-platform/api/v1alpha1"
)

// Integration tests run against a real kube-apiserver and etcd started by envtest.
// They need KUBEBUILDER_ASSETS (set by `make test`). Without it they are skipped
// loudly and only the pure unit tests run.
var (
	testEnv      *envtest.Environment
	testCfg      *rest.Config
	k8sClient    client.Client // talks straight to the API server, no cache
	testScheme   *runtime.Scheme
	envtestReady bool
	nsCounter    atomic.Int64
)

func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintln(os.Stderr, "scheme:", err)
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintln(os.Stderr, "scheme:", err)
		os.Exit(1)
	}

	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		testEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
		}
		cfg, err := testEnv.Start()
		if err != nil {
			fmt.Fprintln(os.Stderr, "envtest failed to start:", err)
			os.Exit(1)
		}
		testCfg = cfg
		k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
		if err != nil {
			fmt.Fprintln(os.Stderr, "client:", err)
			_ = testEnv.Stop()
			os.Exit(1)
		}
		envtestReady = true
	} else {
		fmt.Fprintln(os.Stderr, "WARNING: KUBEBUILDER_ASSETS is not set; integration tests will be SKIPPED. Use `make test`.")
	}

	code := m.Run()
	if testEnv != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if !envtestReady {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make test` to include integration tests")
	}
}

func bg() context.Context { return context.Background() }

// newNamespace creates a fresh namespace. Namespaces are never deleted: envtest
// has no namespace controller, so deletion would hang.
func newNamespace(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("m1-%d-%d", os.Getpid(), nsCounter.Add(1))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(bg(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return name
}

func createISVC(t *testing.T, ns, name string, mutate func(*v1alpha1.InferenceService)) *v1alpha1.InferenceService {
	t.Helper()
	isvc := &v1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.InferenceServiceSpec{
			Model:   "fake-llm",
			Backend: v1alpha1.BackendFake,
			Image:   "fakebackend:dev",
		},
	}
	if mutate != nil {
		mutate(isvc)
	}
	if err := k8sClient.Create(bg(), isvc); err != nil {
		t.Fatalf("create InferenceService: %v", err)
	}
	return isvc
}

func updateISVC(t *testing.T, key types.NamespacedName, mutate func(*v1alpha1.InferenceService)) {
	t.Helper()
	isvc := getISVC(t, key)
	mutate(isvc)
	if err := k8sClient.Update(bg(), isvc); err != nil {
		t.Fatalf("update InferenceService: %v", err)
	}
}

func getISVC(t *testing.T, key types.NamespacedName) *v1alpha1.InferenceService {
	t.Helper()
	var isvc v1alpha1.InferenceService
	if err := k8sClient.Get(bg(), key, &isvc); err != nil {
		t.Fatalf("get InferenceService %s: %v", key, err)
	}
	return &isvc
}

func getDeployment(t *testing.T, key types.NamespacedName) *appsv1.Deployment {
	t.Helper()
	var dep appsv1.Deployment
	if err := k8sClient.Get(bg(), key, &dep); err != nil {
		t.Fatalf("get Deployment %s: %v", key, err)
	}
	return &dep
}

func getService(t *testing.T, key types.NamespacedName) *corev1.Service {
	t.Helper()
	var svc corev1.Service
	if err := k8sClient.Get(bg(), key, &svc); err != nil {
		t.Fatalf("get Service %s: %v", key, err)
	}
	return &svc
}

func newReconciler(c client.Client) *InferenceServiceReconciler {
	return &InferenceServiceReconciler{Client: c, Scheme: testScheme}
}

func reconcileOnce(t *testing.T, r *InferenceServiceReconciler, key types.NamespacedName) {
	t.Helper()
	if _, err := r.Reconcile(bg(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile %s: %v", key, err)
	}
}

// setDeploymentStatus plays the part of the Deployment controller, which envtest
// does not run.
func setDeploymentStatus(t *testing.T, key types.NamespacedName, mutate func(*appsv1.DeploymentStatus)) {
	t.Helper()
	dep := getDeployment(t, key)
	dep.Status.ObservedGeneration = dep.Generation
	mutate(&dep.Status)
	if err := k8sClient.Status().Update(bg(), dep); err != nil {
		t.Fatalf("update Deployment status: %v", err)
	}
}

// createPod plays the part of the scheduler and kubelet.
func createPod(t *testing.T, ns, instance, name string, status corev1.PodStatus) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{labelInstance: instance}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: "x"}}},
	}
	if err := k8sClient.Create(bg(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status = status
	if err := k8sClient.Status().Update(bg(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for: %s", timeout, what)
}

// managerOwnsField reports whether the named field manager owns spec.<field>
// (for example "f:replicas") according to managedFields.
func managerOwnsField(mfs []metav1.ManagedFieldsEntry, manager, field string) bool {
	for _, mf := range mfs {
		if mf.Manager != manager || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(mf.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		if spec, ok := fields["f:spec"].(map[string]interface{}); ok {
			if _, ok := spec[field]; ok {
				return true
			}
		}
	}
	return false
}

// countingClient counts write calls so tests can prove the steady state is quiet.
type countingClient struct {
	client.Client
	patches, creates, updates, deletes, statusWrites atomic.Int64
}

type writeCounts struct{ patches, creates, updates, deletes, statusWrites int64 }

func (c *countingClient) snapshot() writeCounts {
	return writeCounts{c.patches.Load(), c.creates.Load(), c.updates.Load(), c.deletes.Load(), c.statusWrites.Load()}
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches.Add(1)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *countingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates.Add(1)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates.Add(1)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *countingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.deletes.Add(1)
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *countingClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	parent *countingClient
}

func (w *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.statusWrites.Add(1)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.parent.statusWrites.Add(1)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func reconcileRequest(key types.NamespacedName) ctrl.Request {
	return ctrl.Request{NamespacedName: key}
}
