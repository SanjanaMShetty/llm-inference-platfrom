package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Backend selects the model server implementation.
type Backend string

const (
	// BackendVLLM runs vLLM. It is the only GPU-capable backend.
	BackendVLLM Backend = "vLLM"
	// BackendOllama runs Ollama on CPU, for laptop demos.
	BackendOllama Backend = "Ollama"
	// BackendFake runs this repo's fake OpenAI-compatible server (see cmd/fakebackend).
	BackendFake Backend = "fake"
)

// Classification is the sensitivity of the data a service may handle.
type Classification string

const (
	ClassificationPublic     Classification = "Public"
	ClassificationInternal   Classification = "Internal"
	ClassificationRestricted Classification = "Restricted"
)

// Phase is a coarse summary of what the cluster is actually doing for a service.
type Phase string

const (
	// PhasePending: the operator has not created the Deployment yet.
	PhasePending Phase = "Pending"
	// PhaseLoading: pods exist or are starting; none is ready yet.
	PhaseLoading Phase = "Loading"
	// PhaseReady: at least one replica is ready and no pod reports a problem.
	PhaseReady Phase = "Ready"
	// PhaseDegraded: at least one replica is ready, but some pod reports a problem.
	PhaseDegraded Phase = "Degraded"
	// PhaseFailing: no replica is ready and pods report a failure (crash loop, image pull, ...).
	PhaseFailing Phase = "Failing"
	// PhaseUnschedulable: no replica is ready and the scheduler cannot place a pod.
	PhaseUnschedulable Phase = "Unschedulable"
)

// Condition types reported in status.conditions.
const (
	// ConditionReady is True only when the phase is Ready.
	ConditionReady = "Ready"
	// ConditionAvailable is True when at least one replica is ready to serve.
	ConditionAvailable = "Available"
	// ConditionReconciled is True when the operator last applied its desired state successfully.
	ConditionReconciled = "Reconciled"
	// ConditionAcceleratorApplied reports whether the accelerator configuration took effect.
	ConditionAcceleratorApplied = "AcceleratorApplied"
)

// InferenceServiceSpec is the desired state of a served model.
//
// +kubebuilder:validation:XValidation:rule="self.classification != 'Restricted' || self.backend == 'vLLM'",message="Restricted data requires a GPU-capable backend (vLLM); Ollama and fake are CPU-only"
// +kubebuilder:validation:XValidation:rule="self.classification != 'Restricted' || (has(self.accelerator) && has(self.accelerator.deviceClassName))",message="Restricted data requires accelerator.deviceClassName"
type InferenceServiceSpec struct {
	// Model identifies the model to serve: a Hugging Face repo id for vLLM,
	// a model tag for Ollama, or any label for the fake backend.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model"`

	// Backend selects the model server implementation.
	//
	// +kubebuilder:validation:Enum=vLLM;Ollama;fake
	Backend Backend `json:"backend"`

	// Image is the container image of the model server. It is required and has no
	// default so that no image tag is ever chosen on your behalf.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// Classification is the highest data sensitivity this service may handle.
	//
	// +kubebuilder:validation:Enum=Public;Internal;Restricted
	// +kubebuilder:default=Internal
	// +optional
	Classification Classification `json:"classification,omitempty"`

	// ExtraArgs are appended to the model server's arguments.
	//
	// +kubebuilder:validation:MaxItems=32
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// Resources are CPU and memory hints for the model server container.
	//
	// +optional
	Resources *ResourceHints `json:"resources,omitempty"`

	// Autoscaling bounds the replica count. The operator uses minReplicas as the
	// initial replica count only; afterwards an autoscaler owns spec.replicas.
	//
	// +kubebuilder:default={minReplicas:1,maxReplicas:1}
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`

	// Accelerator describes the GPU request. It is validated now and acted on by
	// the DRA milestone; until then the operator reports AcceleratorApplied=False.
	//
	// +optional
	Accelerator *AcceleratorSpec `json:"accelerator,omitempty"`

	// WeightLoadTimeSeconds is how long the model server may take to load weights
	// before it can pass its health check. It sizes the startup probe.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=300
	// +optional
	WeightLoadTimeSeconds int32 `json:"weightLoadTimeSeconds,omitempty"`

	// CostPerHour is the cost of one replica per hour (for example "2.50"). It is
	// stored for the gateway's cost ledger and not used by the operator.
	//
	// +optional
	CostPerHour *resource.Quantity `json:"costPerHour,omitempty"`
}

// ResourceHints are container resource requests.
type ResourceHints struct {
	// CPU is the CPU request.
	// +optional
	CPU *resource.Quantity `json:"cpu,omitempty"`
	// Memory is the memory request; it is also used as the memory limit.
	// +optional
	Memory *resource.Quantity `json:"memory,omitempty"`
}

// AutoscalingSpec bounds the replica count.
//
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= self.minReplicas",message="maxReplicas must be greater than or equal to minReplicas"
type AutoscalingSpec struct {
	// MinReplicas is the lower bound, and the initial replica count.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`
}

// AcceleratorSpec describes how a service obtains GPUs.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.deviceClassName) && has(self.sharedClaimName))",message="deviceClassName and sharedClaimName are mutually exclusive"
type AcceleratorSpec struct {
	// DeviceClassName requests devices of this class through a per-service claim.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	DeviceClassName string `json:"deviceClassName,omitempty"`

	// SharedClaimName references a pre-created claim shared between services.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SharedClaimName string `json:"sharedClaimName,omitempty"`
}

// InferenceServiceStatus reports what the cluster is actually doing.
type InferenceServiceStatus struct {
	// Phase summarizes the observed state.
	//
	// +kubebuilder:validation:Enum=Pending;Loading;Ready;Degraded;Failing;Unschedulable
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation the operator last applied.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the replica count currently set on the Deployment.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of replicas passing their readiness probe.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Endpoint is the in-cluster base URL of the OpenAI-compatible API.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Conditions carry the detail behind the phase, including the scheduler's or
	// kubelet's reason when something is wrong.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// InferenceService declares one served model and the machinery around it.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=llmsvc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="metadata.name must be a DNS label (lowercase letters, digits and '-', at most 63 characters) because it also names the Service"
type InferenceService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceServiceSpec   `json:"spec"`
	Status InferenceServiceStatus `json:"status,omitempty"`
}

// InferenceServiceList is a list of InferenceService.
//
// +kubebuilder:object:root=true
type InferenceServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&InferenceService{}, &InferenceServiceList{})
}
