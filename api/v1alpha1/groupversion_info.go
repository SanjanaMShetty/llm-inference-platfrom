// Package v1alpha1 contains the API types for inference.llmplatform.dev/v1alpha1.
//
// +kubebuilder:object:generate=true
// +groupName=inference.llmplatform.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "inference.llmplatform.dev", Version: "v1alpha1"}

	// SchemeBuilder is used to add Go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group and version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
