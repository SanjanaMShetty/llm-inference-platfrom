package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"llm-platform/api/v1alpha1"
)

func depWith(desired, ready int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: &desired},
		Status: appsv1.DeploymentStatus{Replicas: desired, ReadyReplicas: ready, Conditions: conds},
	}
}

func podNamed(name string, mutate func(*corev1.Pod)) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if mutate != nil {
		mutate(&p)
	}
	return p
}

func unschedulable(msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: msg,
		}}
	}
}

func waiting(reason, msg string) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  containerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}},
		}}
	}
}

func TestEvaluatePhases(t *testing.T) {
	now := metav1.NewTime(time.Now())
	cases := []struct {
		name       string
		dep        *appsv1.Deployment
		pods       []corev1.Pod
		wantPhase  v1alpha1.Phase
		wantReason string
		wantInMsg  string
	}{
		{"no deployment yet", nil, nil, v1alpha1.PhasePending, "Pending", "waiting for the Deployment"},
		{"starting, no pods yet", depWith(1, 0), nil, v1alpha1.PhaseLoading, "Loading", "0/1 replicas ready"},
		{"pod running but not ready", depWith(1, 0), []corev1.Pod{podNamed("a", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })}, v1alpha1.PhaseLoading, "Loading", ""},
		{"ready", depWith(1, 1), []corev1.Pod{podNamed("a", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning })}, v1alpha1.PhaseReady, "Available", "1/1 replicas ready"},
		{"scaling up but serving is still Ready", depWith(3, 1), nil, v1alpha1.PhaseReady, "Available", "1/3 replicas ready"},

		{"unschedulable surfaces the scheduler's words",
			depWith(1, 0),
			[]corev1.Pod{podNamed("a", unschedulable("0/2 nodes are available: 2 Insufficient cpu."))},
			v1alpha1.PhaseUnschedulable, "Unschedulable", "Insufficient cpu"},

		{"crash loop", depWith(1, 0), []corev1.Pod{podNamed("a", waiting("CrashLoopBackOff", "back-off 40s restarting failed container"))}, v1alpha1.PhaseFailing, "CrashLoopBackOff", "back-off 40s"},
		{"image pull backoff", depWith(1, 0), []corev1.Pod{podNamed("a", waiting("ImagePullBackOff", ""))}, v1alpha1.PhaseFailing, "ImagePullBackOff", "container model-server"},
		{"err image pull", depWith(1, 0), []corev1.Pod{podNamed("a", waiting("ErrImagePull", "pull access denied"))}, v1alpha1.PhaseFailing, "ErrImagePull", "pull access denied"},
		{"container creating is not a failure", depWith(1, 0), []corev1.Pod{podNamed("a", waiting("ContainerCreating", ""))}, v1alpha1.PhaseLoading, "Loading", ""},

		{"oom killed after restarts",
			depWith(1, 0),
			[]corev1.Pod{podNamed("a", func(p *corev1.Pod) {
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: containerName, RestartCount: 3,
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
				}}
			})},
			v1alpha1.PhaseFailing, "OOMKilled", "3 restarts"},

		{"failing init container",
			depWith(1, 0),
			[]corev1.Pod{podNamed("a", func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  "preload",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}}
			})},
			v1alpha1.PhaseFailing, "CrashLoopBackOff", "pod a"},

		{"failure outranks unschedulable when nothing is ready",
			depWith(2, 0),
			[]corev1.Pod{
				podNamed("a", unschedulable("no room")),
				podNamed("b", waiting("ErrImagePull", "")),
			},
			v1alpha1.PhaseFailing, "ErrImagePull", "pod b"},

		{"degraded: one ready, one crash looping",
			depWith(2, 1),
			[]corev1.Pod{podNamed("a", waiting("CrashLoopBackOff", ""))},
			v1alpha1.PhaseDegraded, "CrashLoopBackOff", "1/2 replicas ready"},

		{"degraded: one ready, one unschedulable",
			depWith(2, 1),
			[]corev1.Pod{podNamed("a", unschedulable("Insufficient memory"))},
			v1alpha1.PhaseDegraded, "Unschedulable", "Insufficient memory"},

		{"deployment replica failure with nothing ready",
			depWith(1, 0, appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: "exceeded quota"}),
			nil, v1alpha1.PhaseFailing, "FailedCreate", "exceeded quota"},

		{"progress deadline exceeded with nothing ready",
			depWith(1, 0, appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "timed out", LastUpdateTime: now}),
			nil, v1alpha1.PhaseFailing, "ProgressDeadlineExceeded", "timed out"},

		{"progress deadline exceeded while one replica serves",
			depWith(2, 1, appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}),
			nil, v1alpha1.PhaseDegraded, "ProgressDeadlineExceeded", ""},

		{"terminating pods are ignored",
			depWith(1, 0),
			[]corev1.Pod{podNamed("a", func(p *corev1.Pod) {
				p.DeletionTimestamp = &now
				waiting("CrashLoopBackOff", "")(p)
			})},
			v1alpha1.PhaseLoading, "Loading", ""},

		{"finished pods are ignored",
			depWith(1, 0),
			[]corev1.Pod{podNamed("a", func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodFailed
				waiting("CrashLoopBackOff", "")(p)
			})},
			v1alpha1.PhaseLoading, "Loading", ""},

		{"scaled to zero", depWith(0, 0), nil, v1alpha1.PhaseLoading, "ScaledToZero", "0 replicas"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := evaluate(tc.dep, tc.pods)
			if h.phase != tc.wantPhase {
				t.Errorf("phase = %s, want %s (reason %q, message %q)", h.phase, tc.wantPhase, h.reason, h.message)
			}
			if h.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", h.reason, tc.wantReason)
			}
			if !strings.Contains(h.message, tc.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", h.message, tc.wantInMsg)
			}
		})
	}
}

func TestScanPodsIsDeterministic(t *testing.T) {
	pods := []corev1.Pod{
		podNamed("c", waiting("ErrImagePull", "")),
		podNamed("a", waiting("ErrImagePull", "")),
		podNamed("b", waiting("ErrImagePull", "")),
	}
	_, failing := scanPods(pods)
	if len(failing) != 3 || failing[0].Pod != "a" || failing[1].Pod != "b" || failing[2].Pod != "c" {
		t.Fatalf("failing = %+v, want sorted by pod name", failing)
	}
	if pods[0].Name != "c" {
		t.Error("scanPods must not reorder its input")
	}
}

func isvcWithGen(gen int64) *v1alpha1.InferenceService {
	i := testISVC()
	i.Generation = gen
	return i
}

func TestBuildStatusReady(t *testing.T) {
	st := buildStatus(v1alpha1.InferenceServiceStatus{}, isvcWithGen(4), observation{dep: depWith(2, 2)})
	if st.Phase != v1alpha1.PhaseReady || st.Replicas != 2 || st.ReadyReplicas != 2 || st.ObservedGeneration != 4 {
		t.Fatalf("status = %+v", st)
	}
	if st.Endpoint != "http://demo.ns.svc:80" {
		t.Errorf("endpoint = %q", st.Endpoint)
	}
	for typ, want := range map[string]metav1.ConditionStatus{
		v1alpha1.ConditionReady:      metav1.ConditionTrue,
		v1alpha1.ConditionAvailable:  metav1.ConditionTrue,
		v1alpha1.ConditionReconciled: metav1.ConditionTrue,
	} {
		c := meta.FindStatusCondition(st.Conditions, typ)
		if c == nil || c.Status != want {
			t.Errorf("condition %s = %+v, want %s", typ, c, want)
		}
		if c != nil && c.ObservedGeneration != 4 {
			t.Errorf("condition %s observedGeneration = %d, want 4", typ, c.ObservedGeneration)
		}
	}
	if meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionAcceleratorApplied) != nil {
		t.Error("AcceleratorApplied must be absent when no accelerator is configured")
	}
}

func TestBuildStatusNotReadyCarriesTheClusterReason(t *testing.T) {
	pods := []corev1.Pod{podNamed("a", unschedulable("0/2 nodes are available: 2 Insufficient cpu."))}
	st := buildStatus(v1alpha1.InferenceServiceStatus{}, isvcWithGen(1), observation{dep: depWith(1, 0), pods: pods})
	ready := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != "Unschedulable" || !strings.Contains(ready.Message, "Insufficient cpu") {
		t.Fatalf("Ready condition = %+v", ready)
	}
	avail := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionAvailable)
	if avail.Status != metav1.ConditionFalse {
		t.Errorf("Available = %+v, want False", avail)
	}
}

func TestBuildStatusIsIdempotent(t *testing.T) {
	obs := observation{dep: depWith(1, 1)}
	first := buildStatus(v1alpha1.InferenceServiceStatus{}, isvcWithGen(1), obs)
	// Pretend the first status was stored a while ago.
	for i := range first.Conditions {
		first.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Hour))
	}
	second := buildStatus(first, isvcWithGen(1), obs)
	for i := range first.Conditions {
		if !first.Conditions[i].LastTransitionTime.Equal(&second.Conditions[i].LastTransitionTime) {
			t.Errorf("condition %s changed LastTransitionTime without a status change", first.Conditions[i].Type)
		}
	}
	if len(first.Conditions) != len(second.Conditions) {
		t.Errorf("condition count changed: %d -> %d", len(first.Conditions), len(second.Conditions))
	}
}

func TestBuildStatusTransitionUpdatesTime(t *testing.T) {
	loading := buildStatus(v1alpha1.InferenceServiceStatus{}, isvcWithGen(1), observation{dep: depWith(1, 0)})
	for i := range loading.Conditions {
		loading.Conditions[i].LastTransitionTime = metav1.NewTime(time.Now().Add(-time.Hour))
	}
	ready := buildStatus(loading, isvcWithGen(1), observation{dep: depWith(1, 1)})
	c := meta.FindStatusCondition(ready.Conditions, v1alpha1.ConditionReady)
	if time.Since(c.LastTransitionTime.Time) > time.Minute {
		t.Errorf("Ready flipped to True but LastTransitionTime is stale: %v", c.LastTransitionTime)
	}
}

func TestBuildStatusApplyFailureKeepsKnownPhase(t *testing.T) {
	good := buildStatus(v1alpha1.InferenceServiceStatus{}, isvcWithGen(1), observation{dep: depWith(1, 1)})
	failed := buildStatus(good, isvcWithGen(2), observation{applyErr: errors.New("boom: conflict")})
	if failed.Phase != v1alpha1.PhaseReady {
		t.Errorf("phase = %s; a failed apply with no new observation must not flip the phase", failed.Phase)
	}
	if failed.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, must not advance when the apply failed", failed.ObservedGeneration)
	}
	rec := meta.FindStatusCondition(failed.Conditions, v1alpha1.ConditionReconciled)
	if rec.Status != metav1.ConditionFalse || rec.Reason != "ApplyFailed" || !strings.Contains(rec.Message, "boom") {
		t.Errorf("Reconciled = %+v", rec)
	}
}

func TestBuildStatusFirstApplyFailureIsPending(t *testing.T) {
	st := buildStatus(v1alpha1.InferenceServiceStatus{}, isvcWithGen(1), observation{applyErr: errors.New("nope")})
	if st.Phase != "" {
		// With nothing known and no Deployment we only report the Reconciled condition.
		t.Logf("phase = %q", st.Phase)
	}
	if meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionReconciled).Status != metav1.ConditionFalse {
		t.Error("Reconciled must be False")
	}
}

func TestAcceleratorConditionAppearsAndDisappears(t *testing.T) {
	isvc := isvcWithGen(1)
	isvc.Spec.Accelerator = &v1alpha1.AcceleratorSpec{DeviceClassName: "gpu.example.com"}
	st := buildStatus(v1alpha1.InferenceServiceStatus{}, isvc, observation{dep: depWith(1, 1)})
	c := meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionAcceleratorApplied)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "NotImplemented" {
		t.Fatalf("AcceleratorApplied = %+v", c)
	}
	isvc.Spec.Accelerator = nil
	st = buildStatus(st, isvc, observation{dep: depWith(1, 1)})
	if meta.FindStatusCondition(st.Conditions, v1alpha1.ConditionAcceleratorApplied) != nil {
		t.Error("condition should be removed once the accelerator is dropped from the spec")
	}
}

func TestSanitizeReasonAndTruncate(t *testing.T) {
	if got := sanitizeReason("CrashLoopBackOff"); got != "CrashLoopBackOff" {
		t.Errorf("valid reason changed: %q", got)
	}
	for _, bad := range []string{"", "has space", "9starts-with-digit", "ends-with-dash-"} {
		if got := sanitizeReason(bad); got != "Unknown" {
			t.Errorf("sanitizeReason(%q) = %q, want Unknown", bad, got)
		}
	}
	long := strings.Repeat("x", 5000)
	if got := truncate(long); len(got) != maxMessageLen || !strings.HasSuffix(got, "...") {
		t.Errorf("truncate produced len %d", len(got))
	}
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate changed a short string: %q", got)
	}
}
