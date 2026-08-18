// Package v1alpha1 contains the CubePilot platform API types (design doc
// CubePilot-Cloud-for-Agents-Design.md §3): Agent / AgentInstance /
// Capability / TaskTemplate / Task / TaskRun.
//
// Group: assistant.suanova.io — the platform-layer (Cloud for Agents) API.

// +kubebuilder:object:generate=true
// +groupName=assistant.suanova.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version of the CubePilot platform API.
var GroupVersion = schema.GroupVersion{Group: "assistant.suanova.io", Version: "v1alpha1"}

// SchemeBuilder collects the registered types for this group/version.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group/version to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	return SchemeBuilder.AddToScheme(s)
}
