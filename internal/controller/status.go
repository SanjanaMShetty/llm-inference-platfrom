package controller

import (
	"fmt"
	"regexp"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"llm-platform/api/v1alpha1"
)

const maxMessageLen = 1024

// failingWaitingReasons are container waiting reasons that mean "this will not
// start by itself".
var failingWaitingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

var reasonPattern = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// problem is one thing the cluster told us is wrong.
type problem struct {
	Pod     string
	Reason  string
	Message string
}

func (p problem) describe() string {
	msg := p.Reason
	if p.Message != "" {
		msg = p.Reason + ": " + p.Message
	}
	if p.Pod != "" {
		return fmt.Sprintf("pod %s: %s", p.Pod, msg)
	}
	return msg
}

// health is the result of looking at a Deployment and its pods.
type health struct {
	phase   v1alpha1.Phase
	desired int32
	ready   int32
	reason  string
	message string
}

// observation is everything the status is computed from.
type observation struct {
	dep      *appsv1.Deployment
	pods     []corev1.Pod
	applyErr error
}

// evaluate turns observed cluster state into a phase. It is a pure function so it
// can be tested without a cluster.
func evaluate(dep *appsv1.Deployment, pods []corev1.Pod) health {
	if dep == nil {
		return health{phase: v1alpha1.PhasePending, reason: "Pending", message: "waiting for the Deployment to be created"}
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	ready := dep.Status.ReadyReplicas
	h := health{desired: desired, ready: ready}
	counts := fmt.Sprintf("%d/%d replicas ready", ready, desired)

	unsched, failing := scanPods(pods)
	depProblem := deploymentProblem(dep)

	if ready == 0 {
		switch {
		case desired == 0:
			h.phase, h.reason, h.message = v1alpha1.PhaseLoading, "ScaledToZero", "the Deployment is scaled to 0 replicas"
		case len(failing) > 0:
			h.phase, h.reason, h.message = v1alpha1.PhaseFailing, failing[0].Reason, failing[0].describe()
		case depProblem != nil:
			h.phase, h.reason, h.message = v1alpha1.PhaseFailing, depProblem.Reason, depProblem.describe()
		case len(unsched) > 0:
			h.phase, h.reason, h.message = v1alpha1.PhaseUnschedulable, unsched[0].Reason, unsched[0].describe()
		default:
			h.phase, h.reason, h.message = v1alpha1.PhaseLoading, "Loading", counts+"; waiting for the model server to start"
		}
		return h
	}

	var first *problem
	switch {
	case len(failing) > 0:
		first = &failing[0]
	case depProblem != nil:
		first = depProblem
	case len(unsched) > 0:
		first = &unsched[0]
	}
	if first != nil {
		h.phase, h.reason, h.message = v1alpha1.PhaseDegraded, first.Reason, counts+"; "+first.describe()
		return h
	}
	h.phase, h.reason, h.message = v1alpha1.PhaseReady, "Available", counts
	return h
}

// scanPods finds pods that cannot be scheduled or that are failing to run.
// Terminating and finished pods are ignored. Results are sorted by pod name.
func scanPods(pods []corev1.Pod) (unsched, failing []problem) {
	sorted := append([]corev1.Pod(nil), pods...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := range sorted {
		p := &sorted[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if pr := containerProblem(p); pr != nil {
			failing = append(failing, *pr)
			continue
		}
		if pr := schedulingProblem(p); pr != nil {
			unsched = append(unsched, *pr)
		}
	}
	return unsched, failing
}

func containerProblem(p *corev1.Pod) *problem {
	statuses := append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...)
	statuses = append(statuses, p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && failingWaitingReasons[w.Reason] {
			msg := w.Message
			if msg == "" {
				msg = "container " + cs.Name
			}
			return &problem{Pod: p.Name, Reason: w.Reason, Message: msg}
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" && cs.RestartCount > 0 && !cs.Ready {
			return &problem{Pod: p.Name, Reason: "OOMKilled", Message: fmt.Sprintf("container %s was killed for exceeding its memory limit (%d restarts)", cs.Name, cs.RestartCount)}
		}
	}
	return nil
}

func schedulingProblem(p *corev1.Pod) *problem {
	if p.Status.Phase != corev1.PodPending {
		return nil
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return &problem{Pod: p.Name, Reason: "Unschedulable", Message: c.Message}
		}
	}
	return nil
}

func deploymentProblem(dep *appsv1.Deployment) *problem {
	for _, c := range dep.Status.Conditions {
		switch {
		case c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue:
			reason := c.Reason
			if reason == "" {
				reason = "ReplicaFailure"
			}
			return &problem{Reason: reason, Message: c.Message}
		case c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded":
			return &problem{Reason: c.Reason, Message: c.Message}
		}
	}
	return nil
}

// buildStatus computes the new status from the old one and what was observed. It
// never touches the cluster. LastTransitionTime of a condition only changes when
// its status changes, so an unchanged cluster yields an identical status and the
// reconciler can skip the write.
func buildStatus(old v1alpha1.InferenceServiceStatus, isvc *v1alpha1.InferenceService, obs observation) v1alpha1.InferenceServiceStatus {
	st := *old.DeepCopy()
	gen := isvc.Generation

	setReconciled(&st, gen, obs.applyErr)

	if obs.dep == nil && old.Phase != "" {
		// The apply failed before we saw the Deployment; keep what we knew rather
		// than flipping the phase back to Pending.
		return st
	}

	h := evaluate(obs.dep, obs.pods)
	st.Phase = h.phase
	st.Replicas = h.desired
	st.ReadyReplicas = h.ready
	st.Endpoint = endpointFor(isvc)
	if obs.applyErr == nil {
		st.ObservedGeneration = gen
	}

	ready := metav1.ConditionFalse
	if h.phase == v1alpha1.PhaseReady {
		ready = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             ready,
		Reason:             sanitizeReason(h.reason),
		Message:            truncate(h.message),
		ObservedGeneration: gen,
	})

	available := metav1.ConditionFalse
	availReason := "NoReplicasAvailable"
	if h.ready >= 1 {
		available, availReason = metav1.ConditionTrue, "MinimumReplicasAvailable"
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionAvailable,
		Status:             available,
		Reason:             availReason,
		Message:            fmt.Sprintf("%d/%d replicas ready", h.ready, h.desired),
		ObservedGeneration: gen,
	})

	if isvc.Spec.Accelerator != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type:               v1alpha1.ConditionAcceleratorApplied,
			Status:             metav1.ConditionFalse,
			Reason:             "NotImplemented",
			Message:            "accelerator settings are validated but no device claims are created until the DRA milestone",
			ObservedGeneration: gen,
		})
	} else {
		meta.RemoveStatusCondition(&st.Conditions, v1alpha1.ConditionAcceleratorApplied)
	}
	return st
}

func setReconciled(st *v1alpha1.InferenceServiceStatus, gen int64, applyErr error) {
	cond := metav1.Condition{
		Type:               v1alpha1.ConditionReconciled,
		Status:             metav1.ConditionTrue,
		Reason:             "ApplySucceeded",
		Message:            "the Deployment and Service match the spec",
		ObservedGeneration: gen,
	}
	if applyErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ApplyFailed"
		cond.Message = truncate(applyErr.Error())
	}
	meta.SetStatusCondition(&st.Conditions, cond)
}

func sanitizeReason(r string) string {
	if reasonPattern.MatchString(r) && len(r) <= 1024 {
		return r
	}
	return "Unknown"
}

func truncate(s string) string {
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen-3] + "..."
}
