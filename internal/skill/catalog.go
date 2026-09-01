// Package skill implements the skill catalog (design cubepilot-design.md
// §3.4 / §A.1, two layers):
//
//	generic -- platform-provided tools (list-kinds / describe-kind /
//	         resource-manager / kubectl-raw), zero registration;
//	skill   -- Skill (type: atomic + override + target + semantics/security,
//	         thin overlays bound to a CRD; or type: domain + uses[] +
//	         instructions, domain knowledge).
//
// The generic layer is where "the runtime understands the platform
// automatically" lands: the platform reads all
// CRD schemas at startup and serves them to the LLM via list-kinds /
// describe-kind; resource-manager validates data against the CRD schema and
// renders manifests mechanically (schema-driven, zero guessing).
package skill

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// Generic tool names (platform-provided, not registered as Skills).
const (
	ToolListKinds       = "list-kinds"
	ToolDescribeKind    = "describe-kind"
	ToolResourceManager = "resource-manager"
	ToolKubectlRaw      = "kubectl-raw"
)

// GenericTools are the always-available generic tools (design §3.3.1 generic
// layer).
var GenericTools = []string{ToolListKinds, ToolDescribeKind, ToolResourceManager, ToolKubectlRaw}

// CRDSchema is one discovered CRD's schema (design §3.3.1: the platform
// reads all CRD schemas into a cache at startup; list-kinds / describe-kind
// discover them dynamically on use).
type CRDSchema struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	// Plural is the resource plural (e.g. devenvironments).
	Plural string `json:"plural"`
	// Description is the CRD's metadata description (annotations or kind name).
	Description string `json:"description,omitempty"`
	// OpenAPI is the structural schema JSON (from CRD validation; nil when the
	// CRD carries no schema).
	OpenAPI map[string]any `json:"openapi,omitempty"`
}

// Catalog is the platform's skill catalog: discovered CRD schemas plus
// the registered Skill CRs. It answers "what an Agent can use" (design
// §3.1).
type Catalog struct {
	discovery discovery.DiscoveryInterface
	dynamic   dynamic.Interface
	config    *rest.Config

	// schemas is the cached CRD schema table (group/kind -> schema).
	schemas map[string]*CRDSchema
}

// NewCatalog builds a Catalog over the given REST config. The CRD schema table
// is lazily loaded (design §4.2.1: the platform reads all CRD schemas into a
// cache at startup).
func NewCatalog(cfg *rest.Config) (*Catalog, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Catalog{
		discovery: disc,
		dynamic:   dyn,
		config:    cfg,
		schemas:   map[string]*CRDSchema{},
	}, nil
}

// Refresh re-discovers the CRD schema table (generic toolset change
// governance: CRD add/update/delete -> the toolset changes automatically;
// design §3.3.1 toolset change governance).
func (c *Catalog) Refresh(ctx context.Context) error {
	list, err := c.discovery.ServerPreferredResources()
	if err != nil {
		// Partial discovery errors are common (some groups fail); keep what we got.
		if list == nil {
			return fmt.Errorf("discover resources: %w", err)
		}
	}
	byKey := map[string]*CRDSchema{}
	for _, rl := range list {
		gv, err := schema.ParseGroupVersion(rl.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range rl.APIResources {
			if !strings.Contains(r.Name, "/") && (strings.HasSuffix(r.Name, "s") || r.Kind != "") {
				key := gv.Group + "/" + r.Kind
				if _, ok := byKey[key]; ok {
					continue
				}
				byKey[key] = &CRDSchema{
					Group:       gv.Group,
					Version:     gv.Version,
					Kind:        r.Kind,
					Plural:      r.Name,
					Description: fmt.Sprintf("%s (%s)", r.Kind, gv.Group),
				}
			}
		}
	}
	c.schemas = byKey
	return nil
}

// Schemas returns the current CRD schema table (group/kind -> schema), sorted
// by kind for stable output.
func (c *Catalog) Schemas() []*CRDSchema {
	out := make([]*CRDSchema, 0, len(c.schemas))
	for _, s := range c.schemas {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// SchemaFor returns the CRD schema for (group, kind), or nil.
func (c *Catalog) SchemaFor(group, kind string) *CRDSchema {
	if group == "" {
		group = "ai.cubestack.io"
	}
	return c.schemas[group+"/"+kind]
}

// FindKind resolves a kind name (case-insensitive) to a schema.
func (c *Catalog) FindKind(kind string) *CRDSchema {
	lower := strings.ToLower(kind)
	for _, s := range c.schemas {
		if strings.ToLower(s.Kind) == lower {
			return s
		}
	}
	return nil
}

// ValidateSkill validates a Skill registration (design §3.4): the source
// discriminant mirrors the CRD CEL rules (guards the logic without an API
// server) and phase 1 admits only Platform visibility.
func (c *Catalog) ValidateSkill(skill *v1alpha1.Skill) error {
	switch skill.Spec.Visibility {
	case v1alpha1.SkillVisibilityPlatform:
		// OK — phase 1 only.
	case v1alpha1.SkillVisibilityTenant, v1alpha1.SkillVisibilityUser:
		return fmt.Errorf("skill %s: visibility %q is phase 2 (only Platform in phase 1)", skill.Name, skill.Spec.Visibility)
	default:
		return fmt.Errorf("skill %s: invalid visibility %q", skill.Name, skill.Spec.Visibility)
	}
	switch skill.Spec.Source.Type {
	case v1alpha1.SkillSourcePath:
		if skill.Spec.Source.Path == "" {
			return fmt.Errorf("skill %s: source.type=Path requires source.path", skill.Name)
		}
		if skill.Spec.Source.S3 != nil {
			return fmt.Errorf("skill %s: source.type=Path forbids source.s3", skill.Name)
		}
	case v1alpha1.SkillSourceS3:
		return fmt.Errorf("skill %s: source.type=S3 is phase 2", skill.Name)
	default:
		return fmt.Errorf("skill %s: invalid source.type %q", skill.Name, skill.Spec.Source.Type)
	}
	return nil
}

// ToolSetForAgent computes the effective tool set for an AgentTemplate
// definition: generic tools are always available; each referenced Skill
// (all Platform-visible in phase 1) contributes its name (design §3.4).
func ToolSetForAgent(agent *v1alpha1.AgentTemplate, skills []v1alpha1.Skill) []string {
	set := map[string]bool{}
	for _, t := range GenericTools {
		set[t] = true
	}
	for _, ref := range agent.Spec.Skills {
		for _, skill := range skills {
			if skill.Name == ref {
				set[ref] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// CRDExists reports whether a CRD (group/kind) is present (for Skill
// validation against a live cluster; returns false on any error).
func (c *Catalog) CRDExists(group, kind string) bool {
	return c.SchemaFor(group, kind) != nil
}

// resourceFor returns the dynamic resource client for a schema.
func (c *Catalog) resourceFor(s *CRDSchema) dynamic.NamespaceableResourceInterface {
	gvr := schema.GroupVersionResource{Group: s.Group, Version: s.Version, Resource: s.Plural}
	return c.dynamic.Resource(gvr)
}

// List lists instances of a CRD kind (used by resource-manager read paths).
func (c *Catalog) List(ctx context.Context, kind, namespace string) (*unstructured.UnstructuredList, error) {
	s := c.FindKind(kind)
	if s == nil {
		return nil, fmt.Errorf("unknown kind %q (use list-kinds to discover)", kind)
	}
	ri := c.resourceFor(s)
	if namespace != "" {
		return ri.Namespace(namespace).List(ctx, metav1.ListOptions{})
	}
	return ri.List(ctx, metav1.ListOptions{})
}

// Get fetches one instance of a CRD kind by name.
func (c *Catalog) Get(ctx context.Context, kind, namespace, name string) (*unstructured.Unstructured, error) {
	s := c.FindKind(kind)
	if s == nil {
		return nil, fmt.Errorf("unknown kind %q (use list-kinds to discover)", kind)
	}
	ri := c.resourceFor(s)
	if namespace != "" {
		return ri.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

// Create applies one instance of a CRD kind (write path; phase-one passes
// through directly).
func (c *Catalog) Create(ctx context.Context, kind, namespace string, obj map[string]any) (*unstructured.Unstructured, error) {
	s := c.FindKind(kind)
	if s == nil {
		return nil, fmt.Errorf("unknown kind %q (use list-kinds to discover)", kind)
	}
	u := &unstructured.Unstructured{Object: obj}
	ri := c.resourceFor(s)
	if namespace != "" {
		return ri.Namespace(namespace).Create(ctx, u, metav1.CreateOptions{})
	}
	return ri.Create(ctx, u, metav1.CreateOptions{})
}

// Delete removes one instance of a CRD kind.
func (c *Catalog) Delete(ctx context.Context, kind, namespace, name string) error {
	s := c.FindKind(kind)
	if s == nil {
		return fmt.Errorf("unknown kind %q (use list-kinds to discover)", kind)
	}
	ri := c.resourceFor(s)
	if namespace != "" {
		return ri.Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}
	return ri.Delete(ctx, name, metav1.DeleteOptions{})
}

// IsNotFound reports whether an error is a Kubernetes not-found.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
