// Package v1alpha1 contains the CubePilot platform API types (design doc
// cubepilot-design.md §3): AgentTemplate / AgentInstance / Skill /
// TaskTemplate / Task / TaskRun.
//
// Group: ai.cubestack.io -- the platform-layer (Cloud for Agents) API.

// +kubebuilder:object:generate=true
// +groupName=ai.cubestack.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version of the CubePilot platform API.
var GroupVersion = schema.GroupVersion{Group: "ai.cubestack.io", Version: "v1alpha1"}

// SchemeBuilder collects the registered types for this group/version.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group/version to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	return SchemeBuilder.AddToScheme(s)
}
