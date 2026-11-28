package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"llm-platform/api/v1alpha1"
)

const (
	// labelInstance is the ONLY label in the Deployment selector. The selector is
	// immutable, so it must never depend on anything a user can change.
	labelInstance       = "llm-platform.io/inference-service"
	labelBackend        = "llm-platform.io/backend"
	labelClassification = "llm-platform.io/classification"
	labelManagedBy      = "app.kubernetes.io/managed-by"
	managedByValue      = "llm-platform-operator"

	// containerName is constant across backends so that switching backend edits
	// the one container in place instead of adding a second one.
	containerName = "model-server"
	portName      = "http"
	servicePort   = 80

	probePeriodSeconds       = 5
	defaultWeightLoadSeconds = 300
)

func selectorLabels(name string) map[string]string {
	return map[string]string{labelInstance: name}
}

func objectLabels(isvc *v1alpha1.InferenceService) map[string]string {
	labels := selectorLabels(isvc.Name)
	labels[labelManagedBy] = managedByValue
	labels[labelBackend] = string(isvc.Spec.Backend)
	if isvc.Spec.Classification != "" {
		labels[labelClassification] = string(isvc.Spec.Classification)
	}
	return labels
}

// startupFailureThreshold sizes the startup probe so that failureThreshold x period
// covers the declared weight-load time.
func startupFailureThreshold(weightLoadSeconds int32) int32 {
	if weightLoadSeconds < 1 {
		weightLoadSeconds = defaultWeightLoadSeconds
	}
	return (weightLoadSeconds + probePeriodSeconds - 1) / probePeriodSeconds
}

// buildDeployment returns the desired Deployment.
//
// It deliberately leaves spec.replicas unset. Under server-side apply a field that
// the operator stops mentioning is removed from the object, so the operator must
// never mention replicas here. The initial count is set once, by a different field
// manager (see bootstrapReplicas), and an autoscaler owns it from then on.
func buildDeployment(isvc *v1alpha1.InferenceService) (*appsv1.Deployment, error) {
	spec := &isvc.Spec
	profile, err := profileFor(spec.Backend)
	if err != nil {
		return nil, err
	}

	var args []string
	args = append(args, profile.args(spec)...)
	args = append(args, spec.ExtraArgs...)

	health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path: profile.healthPath,
		Port: intstr.FromString(portName),
	}}

	container := corev1.Container{
		Name:  containerName,
		Image: spec.Image,
		Args:  args,
		Env:   profile.env(spec),
		Ports: []corev1.ContainerPort{{
			Name:          portName,
			ContainerPort: profile.port,
			Protocol:      corev1.ProtocolTCP,
		}},
		// The startup probe holds off liveness and readiness while weights load.
		StartupProbe: &corev1.Probe{
			ProbeHandler:     health,
			PeriodSeconds:    probePeriodSeconds,
			TimeoutSeconds:   3,
			FailureThreshold: startupFailureThreshold(spec.WeightLoadTimeSeconds),
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     health,
			PeriodSeconds:    probePeriodSeconds,
			TimeoutSeconds:   3,
			FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:     health,
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
			FailureThreshold: 3,
		},
		Resources: resourceRequirements(spec),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}

	labels := objectLabels(isvc)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(isvc.Name)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}, nil
}

func resourceRequirements(spec *v1alpha1.InferenceServiceSpec) corev1.ResourceRequirements {
	var rr corev1.ResourceRequirements
	if spec.Resources == nil {
		return rr
	}
	if q := spec.Resources.CPU; q != nil {
		rr.Requests = corev1.ResourceList{corev1.ResourceCPU: *q}
	}
	if q := spec.Resources.Memory; q != nil {
		if rr.Requests == nil {
			rr.Requests = corev1.ResourceList{}
		}
		rr.Requests[corev1.ResourceMemory] = *q
		rr.Limits = corev1.ResourceList{corev1.ResourceMemory: *q}
	}
	return rr
}

// buildService returns the desired ClusterIP Service in front of the pods.
func buildService(isvc *v1alpha1.InferenceService) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      isvc.Name,
			Namespace: isvc.Namespace,
			Labels:    objectLabels(isvc),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(isvc.Name),
			Ports: []corev1.ServicePort{{
				Name:       portName,
				Port:       servicePort,
				TargetPort: intstr.FromString(portName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// endpointFor is the in-cluster base URL of the service's OpenAI-compatible API.
func endpointFor(isvc *v1alpha1.InferenceService) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", isvc.Name, isvc.Namespace, servicePort)
}

// initialReplicas is the replica count used when the Deployment is first created.
func initialReplicas(isvc *v1alpha1.InferenceService) int32 {
	if a := isvc.Spec.Autoscaling; a != nil && a.MinReplicas >= 1 {
		return a.MinReplicas
	}
	return 1
}
