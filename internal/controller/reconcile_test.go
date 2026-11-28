package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"llm-platform/api/v1alpha1"
)

func keyOf(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func TestReconcileCreatesOwnedDeploymentAndService(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	isvc := createISVC(t, ns, "demo", func(i *v1alpha1.InferenceService) {
		i.Spec.WeightLoadTimeSeconds = 120
		i.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: 2, MaxReplicas: 4}
		i.Spec.ExtraArgs = []string{"-first-token-delay=300ms"}
	})
	reconcileOnce(t, newReconciler(k8sClient), keyOf(ns, "demo"))

	dep := getDeployment(t, keyOf(ns, "demo"))
	svc := getService(t, keyOf(ns, "demo"))

	// Selector is exactly the service name.
	if got := dep.Spec.Selector.MatchLabels; len(got) != 1 || got[labelInstance] != "demo" {
		t.Errorf("selector = %v, want only %s=demo", got, labelInstance)
	}

	// Exactly one container, built from the spec.
	cs := dep.Spec.Template.Spec.Containers
	if len(cs) != 1 || cs[0].Name != containerName || cs[0].Image != "fakebackend:dev" {
		t.Fatalf("containers = %+v", cs)
	}
	if len(cs[0].Args) != 1 || cs[0].Args[0] != "-first-token-delay=300ms" {
		t.Errorf("args = %v", cs[0].Args)
	}

	// Startup probe sized from weightLoadTimeSeconds=120 at a 5s period.
	if sp := cs[0].StartupProbe; sp == nil || sp.FailureThreshold != 24 || sp.PeriodSeconds != 5 {
		t.Errorf("startup probe = %+v, want 24 x 5s", sp)
	}
	if cs[0].ReadinessProbe == nil || cs[0].LivenessProbe == nil {
		t.Error("readiness and liveness probes must be set")
	}

	// Initial replicas come from autoscaling.minReplicas.
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2", dep.Spec.Replicas)
	}

	// Both children are controlled by the InferenceService.
	for kind, refs := range map[string][]metav1.OwnerReference{"Deployment": dep.OwnerReferences, "Service": svc.OwnerReferences} {
		if len(refs) != 1 {
			t.Fatalf("%s ownerReferences = %+v", kind, refs)
		}
		ref := refs[0]
		if ref.Kind != "InferenceService" || ref.UID != isvc.UID || ref.Controller == nil || !*ref.Controller ||
			ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
			t.Errorf("%s owner reference = %+v, want a blocking controller reference to the InferenceService", kind, ref)
		}
	}

	// Service points at the named port.
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.String() != portName {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}

	// Status was written.
	got := getISVC(t, keyOf(ns, "demo"))
	if got.Status.Phase != v1alpha1.PhaseLoading || got.Status.Replicas != 2 || got.Status.ObservedGeneration != got.Generation {
		t.Errorf("status = %+v", got.Status)
	}
	if got.Status.Endpoint != fmt.Sprintf("http://demo.%s.svc:80", ns) {
		t.Errorf("endpoint = %q", got.Status.Endpoint)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReconciled); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Reconciled = %+v", c)
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v; none are needed because everything owned is garbage collected", got.Finalizers)
	}
}

func TestReplicasAreOwnedByBootstrapNotByTheOperator(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	createISVC(t, ns, "owners", func(i *v1alpha1.InferenceService) {
		i.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: 2, MaxReplicas: 3}
	})
	r := newReconciler(k8sClient)
	reconcileOnce(t, r, keyOf(ns, "owners"))
	reconcileOnce(t, r, keyOf(ns, "owners"))

	dep := getDeployment(t, keyOf(ns, "owners"))
	if managerOwnsField(dep.ManagedFields, fieldOwner, "f:replicas") {
		t.Error("the main operator field manager must not own spec.replicas")
	}
	if !managerOwnsField(dep.ManagedFields, fieldOwnerBootstrap, "f:replicas") {
		t.Error("the bootstrap field manager should own spec.replicas after creation")
	}
	if !managerOwnsField(dep.ManagedFields, fieldOwner, "f:template") {
		t.Error("the main operator field manager should own the pod template")
	}
}

func TestOperatorNeverFightsTheAutoscaler(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "scaled")
	createISVC(t, ns, "scaled", func(i *v1alpha1.InferenceService) {
		i.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: 2, MaxReplicas: 8}
	})
	r := newReconciler(k8sClient)
	reconcileOnce(t, r, key)

	// An autoscaler (here: another field manager) scales the Deployment to 5.
	dep := getDeployment(t, key)
	raw := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"replicas":5}}`))
	if err := k8sClient.Patch(bg(), dep, raw, client.FieldOwner("fake-autoscaler")); err != nil {
		t.Fatalf("scale: %v", err)
	}

	// A spec change makes the operator re-apply everything.
	updateISVC(t, key, func(i *v1alpha1.InferenceService) {
		i.Spec.ExtraArgs = []string{"-first-token-delay=1s"}
	})
	reconcileOnce(t, r, key)

	dep = getDeployment(t, key)
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 5 {
		t.Fatalf("replicas = %v after a reconcile, want 5: the operator reset what the autoscaler set", dep.Spec.Replicas)
	}
	if got := dep.Spec.Template.Spec.Containers[0].Args; len(got) != 1 || got[0] != "-first-token-delay=1s" {
		t.Errorf("the spec change was not applied: args = %v", got)
	}

	// Lowering minReplicas does not change a running Deployment either.
	updateISVC(t, key, func(i *v1alpha1.InferenceService) {
		i.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: 1, MaxReplicas: 8}
	})
	reconcileOnce(t, r, key)
	dep = getDeployment(t, key)
	if *dep.Spec.Replicas != 5 {
		t.Errorf("replicas = %d, want 5", *dep.Spec.Replicas)
	}
	if managerOwnsField(dep.ManagedFields, fieldOwner, "f:replicas") || managerOwnsField(dep.ManagedFields, fieldOwnerBootstrap, "f:replicas") {
		t.Error("after another manager took spec.replicas, neither operator field manager should still own it")
	}
	if got := getISVC(t, key).Status.Replicas; got != 5 {
		t.Errorf("status.replicas = %d, want 5", got)
	}
}

func TestBackendSwitchLeavesExactlyOneContainer(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "switch")
	createISVC(t, ns, "switch", nil)
	r := newReconciler(k8sClient)
	reconcileOnce(t, r, key)

	type want struct {
		backend   v1alpha1.Backend
		image     string
		model     string
		port      int32
		path      string
		firstArg  string
		envNames  []string
		extraArgs []string
	}
	steps := []want{
		{v1alpha1.BackendVLLM, "vllm/vllm-openai:test", "facebook/opt-125m", 8000, "/health", "facebook/opt-125m", nil, nil},
		{v1alpha1.BackendOllama, "ollama/ollama:test", "llama3.2:1b", 11434, "/", "serve", nil, nil},
		{v1alpha1.BackendFake, "fakebackend:dev", "fake-llm", 8000, "/healthz", "-first-token-delay=1s", []string{"FAKE_MODEL"}, []string{"-first-token-delay=1s"}},
	}
	for _, w := range steps {
		updateISVC(t, key, func(i *v1alpha1.InferenceService) {
			i.Spec.Backend, i.Spec.Image, i.Spec.Model, i.Spec.ExtraArgs = w.backend, w.image, w.model, w.extraArgs
		})
		reconcileOnce(t, r, key)

		dep := getDeployment(t, key)
		cs := dep.Spec.Template.Spec.Containers
		if len(cs) != 1 {
			t.Fatalf("after switching to %s there are %d containers, want exactly 1: %+v", w.backend, len(cs), cs)
		}
		c := cs[0]
		if c.Image != w.image {
			t.Errorf("%s: image = %q, want %q", w.backend, c.Image, w.image)
		}
		if len(c.Ports) != 1 || c.Ports[0].ContainerPort != w.port {
			t.Errorf("%s: ports = %+v, want only %d (a stale port means apply did not prune)", w.backend, c.Ports, w.port)
		}
		if c.ReadinessProbe.HTTPGet.Path != w.path {
			t.Errorf("%s: health path = %q, want %q", w.backend, c.ReadinessProbe.HTTPGet.Path, w.path)
		}
		if len(c.Args) == 0 || c.Args[0] != w.firstArg {
			t.Errorf("%s: args = %v, want first arg %q", w.backend, c.Args, w.firstArg)
		}
		var envNames []string
		for _, e := range c.Env {
			envNames = append(envNames, e.Name)
		}
		if strings.Join(envNames, ",") != strings.Join(w.envNames, ",") {
			t.Errorf("%s: env = %v, want %v (stale env vars mean apply did not prune)", w.backend, envNames, w.envNames)
		}
		if got := dep.Spec.Template.Labels[labelBackend]; got != string(w.backend) {
			t.Errorf("%s: backend label = %q", w.backend, got)
		}
		// The selector must never change; if it did, the Deployment would be rejected.
		if len(dep.Spec.Selector.MatchLabels) != 1 {
			t.Errorf("%s: selector = %v", w.backend, dep.Spec.Selector.MatchLabels)
		}
	}
}

func TestDriftIsCorrected(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "drift")
	createISVC(t, ns, "drift", nil)
	r := newReconciler(k8sClient)
	reconcileOnce(t, r, key)

	// Someone edits the image by hand.
	dep := getDeployment(t, key)
	dep.Spec.Template.Spec.Containers[0].Image = "evil:1"
	if err := k8sClient.Update(bg(), dep); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, key)
	if got := getDeployment(t, key).Spec.Template.Spec.Containers[0].Image; got != "fakebackend:dev" {
		t.Errorf("image = %q, want it reverted to fakebackend:dev", got)
	}

	// Someone deletes the Service.
	if err := k8sClient.Delete(bg(), getService(t, key)); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, key)
	getService(t, key) // fails the test if it was not recreated
}

func TestStatusPhases(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)

	podPending := func(mutate func(*corev1.PodStatus)) corev1.PodStatus {
		s := corev1.PodStatus{Phase: corev1.PodPending}
		mutate(&s)
		return s
	}
	waitingStatus := func(reason, msg string) corev1.PodStatus {
		return podPending(func(s *corev1.PodStatus) {
			s.ContainerStatuses = []corev1.ContainerStatus{{
				Name: containerName, Image: "x",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}},
			}}
		})
	}

	scenarios := []struct {
		name       string
		minRepl    int32
		setup      func(t *testing.T, ns, name string)
		wantPhase  v1alpha1.Phase
		wantReason string
		wantMsg    string
		wantReady  int32
	}{
		{name: "loading until something is ready", minRepl: 1, wantPhase: v1alpha1.PhaseLoading, wantReason: "Loading"},

		{name: "ready", minRepl: 1, wantPhase: v1alpha1.PhaseReady, wantReason: "Available", wantReady: 1,
			setup: func(t *testing.T, ns, name string) {
				setDeploymentStatus(t, keyOf(ns, name), func(s *appsv1.DeploymentStatus) {
					s.Replicas, s.ReadyReplicas, s.AvailableReplicas, s.UpdatedReplicas = 1, 1, 1, 1
				})
			}},

		{name: "unschedulable shows the scheduler's reason", minRepl: 1, wantPhase: v1alpha1.PhaseUnschedulable, wantReason: "Unschedulable", wantMsg: "Insufficient nvidia.com/gpu",
			setup: func(t *testing.T, ns, name string) {
				createPod(t, ns, name, name+"-pod", podPending(func(s *corev1.PodStatus) {
					s.Conditions = []corev1.PodCondition{{
						Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
						Message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu.",
					}}
				}))
			}},

		{name: "crash loop", minRepl: 1, wantPhase: v1alpha1.PhaseFailing, wantReason: "CrashLoopBackOff", wantMsg: "back-off",
			setup: func(t *testing.T, ns, name string) {
				createPod(t, ns, name, name+"-pod", waitingStatus("CrashLoopBackOff", "back-off 40s restarting failed container"))
			}},

		{name: "image pull failure", minRepl: 1, wantPhase: v1alpha1.PhaseFailing, wantReason: "ErrImagePull", wantMsg: "pull access denied",
			setup: func(t *testing.T, ns, name string) {
				createPod(t, ns, name, name+"-pod", waitingStatus("ErrImagePull", "pull access denied"))
			}},

		{name: "degraded: one replica serves, another crash loops", minRepl: 2, wantPhase: v1alpha1.PhaseDegraded, wantReason: "CrashLoopBackOff", wantReady: 1,
			setup: func(t *testing.T, ns, name string) {
				setDeploymentStatus(t, keyOf(ns, name), func(s *appsv1.DeploymentStatus) {
					s.Replicas, s.ReadyReplicas, s.AvailableReplicas, s.UpdatedReplicas = 2, 1, 1, 2
				})
				createPod(t, ns, name, name+"-bad", waitingStatus("CrashLoopBackOff", ""))
			}},

		{name: "replica failure reported by the Deployment controller", minRepl: 1, wantPhase: v1alpha1.PhaseFailing, wantReason: "FailedCreate", wantMsg: "exceeded quota",
			setup: func(t *testing.T, ns, name string) {
				setDeploymentStatus(t, keyOf(ns, name), func(s *appsv1.DeploymentStatus) {
					s.Conditions = []appsv1.DeploymentCondition{{
						Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue,
						Reason: "FailedCreate", Message: "pods are forbidden: exceeded quota",
					}}
				})
			}},
	}

	r := newReconciler(k8sClient)
	for i, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			name := fmt.Sprintf("phase-%d", i)
			key := keyOf(ns, name)
			createISVC(t, ns, name, func(i *v1alpha1.InferenceService) {
				i.Spec.Autoscaling = &v1alpha1.AutoscalingSpec{MinReplicas: sc.minRepl, MaxReplicas: sc.minRepl}
			})
			reconcileOnce(t, r, key)
			if sc.setup != nil {
				sc.setup(t, ns, name)
			}
			reconcileOnce(t, r, key)

			got := getISVC(t, key)
			if got.Status.Phase != sc.wantPhase {
				t.Fatalf("phase = %s, want %s; conditions: %+v", got.Status.Phase, sc.wantPhase, got.Status.Conditions)
			}
			if got.Status.ReadyReplicas != sc.wantReady {
				t.Errorf("readyReplicas = %d, want %d", got.Status.ReadyReplicas, sc.wantReady)
			}
			ready := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
			if ready == nil {
				t.Fatal("no Ready condition")
			}
			if ready.Reason != sc.wantReason {
				t.Errorf("Ready reason = %q, want %q", ready.Reason, sc.wantReason)
			}
			if !strings.Contains(ready.Message, sc.wantMsg) {
				t.Errorf("Ready message = %q, want it to contain %q", ready.Message, sc.wantMsg)
			}
			wantReadyStatus := metav1.ConditionFalse
			if sc.wantPhase == v1alpha1.PhaseReady {
				wantReadyStatus = metav1.ConditionTrue
			}
			if ready.Status != wantReadyStatus {
				t.Errorf("Ready status = %s, want %s", ready.Status, wantReadyStatus)
			}
		})
	}
}

func TestSteadyStateMakesNoEffectiveWrites(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "quiet")
	createISVC(t, ns, "quiet", nil)

	cc := &countingClient{Client: k8sClient}
	r := newReconciler(cc)
	reconcileOnce(t, r, key) // creates everything and writes status
	reconcileOnce(t, r, key) // settles

	versions := func() [3]string {
		return [3]string{
			getISVC(t, key).ResourceVersion,
			getDeployment(t, key).ResourceVersion,
			getService(t, key).ResourceVersion,
		}
	}
	before, snap := versions(), cc.snapshot()

	const rounds = 5
	for i := 0; i < rounds; i++ {
		reconcileOnce(t, r, key)
	}
	after, now := versions(), cc.snapshot()

	if before != after {
		t.Errorf("resourceVersions changed in a steady state: before %v, after %v", before, after)
	}
	d := writeCounts{now.patches - snap.patches, now.creates - snap.creates, now.updates - snap.updates, now.deletes - snap.deletes, now.statusWrites - snap.statusWrites}
	if d.creates != 0 || d.updates != 0 || d.deletes != 0 || d.statusWrites != 0 {
		t.Errorf("steady state wrote: %+v, want no creates, updates, deletes or status writes", d)
	}
	if d.patches != 2*rounds {
		t.Errorf("apply patches = %d over %d reconciles, want %d (one for the Deployment and one for the Service)", d.patches, rounds, 2*rounds)
	}
	t.Logf("steady state, per reconcile: %d server-side apply requests (no-ops: resourceVersion unchanged), %d status writes, %d creates/updates/deletes",
		d.patches/rounds, d.statusWrites/rounds, d.creates+d.updates+d.deletes)
}

func TestDeletionLeavesCleanupToGarbageCollection(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "gone")
	isvc := createISVC(t, ns, "gone", nil)
	r := newReconciler(k8sClient)
	reconcileOnce(t, r, key)

	if len(getISVC(t, key).Finalizers) != 0 {
		t.Fatal("no finalizer should be needed")
	}
	if err := k8sClient.Delete(bg(), isvc); err != nil {
		t.Fatal(err)
	}
	// Deletion completes immediately because nothing blocks it.
	if err := k8sClient.Get(bg(), key, &v1alpha1.InferenceService{}); !apierrors.IsNotFound(err) {
		t.Fatalf("InferenceService still exists after delete: %v", err)
	}
	// Reconciling a deleted object is a quiet no-op.
	reconcileOnce(t, r, key)

	// envtest runs no garbage collector, so the children remain here; in a real
	// cluster the owner references below are what make them disappear.
	for _, refs := range [][]metav1.OwnerReference{getDeployment(t, key).OwnerReferences, getService(t, key).OwnerReferences} {
		if len(refs) != 1 || refs[0].UID != isvc.UID || refs[0].Controller == nil || !*refs[0].Controller {
			t.Errorf("owner references = %+v, want a controller reference to the deleted InferenceService", refs)
		}
	}
}

func TestRestrictedVLLMReportsAcceleratorNotApplied(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "restricted")
	createISVC(t, ns, "restricted", func(i *v1alpha1.InferenceService) {
		i.Spec.Backend = v1alpha1.BackendVLLM
		i.Spec.Model = "facebook/opt-125m"
		i.Spec.Image = "vllm/vllm-openai:test"
		i.Spec.Classification = v1alpha1.ClassificationRestricted
		i.Spec.Accelerator = &v1alpha1.AcceleratorSpec{DeviceClassName: "gpu.example.com"}
	})
	reconcileOnce(t, newReconciler(k8sClient), key)

	c := meta.FindStatusCondition(getISVC(t, key).Status.Conditions, v1alpha1.ConditionAcceleratorApplied)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NotImplemented" {
		t.Fatalf("AcceleratorApplied = %+v, want False/NotImplemented until the DRA milestone", c)
	}
	if got := getDeployment(t, key).Spec.Template.Spec.Containers[0].Args[0]; got != "facebook/opt-125m" {
		t.Errorf("vLLM args[0] = %q", got)
	}
}

// failingDeploymentPatches simulates the API server rejecting Deployment applies.
type failingDeploymentPatches struct{ client.Client }

func (c failingDeploymentPatches) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*appsv1.Deployment); ok {
		return errors.New("simulated API failure")
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestApplyFailureIsReportedAndThenHeals(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	key := keyOf(ns, "flaky")
	createISVC(t, ns, "flaky", nil)

	broken := newReconciler(failingDeploymentPatches{Client: k8sClient})
	if _, err := broken.Reconcile(bg(), reconcileRequest(key)); err == nil {
		t.Fatal("expected the reconcile to return the apply error so it is retried")
	}
	got := getISVC(t, key)
	c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReconciled)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ApplyFailed" || !strings.Contains(c.Message, "simulated API failure") {
		t.Fatalf("Reconciled = %+v, want False/ApplyFailed with the error", c)
	}
	if got.Status.ObservedGeneration != 0 {
		t.Errorf("observedGeneration = %d, must not advance after a failed apply", got.Status.ObservedGeneration)
	}

	reconcileOnce(t, newReconciler(k8sClient), key)
	got = getISVC(t, key)
	if c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReconciled); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Reconciled = %+v after recovery, want True", c)
	}
	if got.Status.Phase != v1alpha1.PhaseLoading {
		t.Errorf("phase = %s after recovery, want Loading", got.Status.Phase)
	}
}
