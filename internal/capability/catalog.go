// Package capability implements the three-layer capability model
// (design doc CubePilot-Cloud-for-Agents-Design.md §3.3.1 / §4.2):
//
//	generic — platform-provided tools (list-kinds / describe-kind /
//	         resource-manager / kubectl-raw), zero registration;
//	atomic  — Capability (type: atomic + override + target + semantics/security),
//	         thin overlays bound to a CRD (never touch fields);
//	domain  — Capability (type: domain + uses[] + instructions), domain knowledge.
//
// The generic layer is the "runtime 自动懂平台" 落地点: the platform reads all
// CRD schemas at startup and serves them to the LLM via list-kinds /
// describe-kind; resource-manager validates data against the CRD schema and
// renders manifests mechanically (schema-driven, 零猜测).
package capability

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

// Generic tool names (platform-provided, not registered as Capabilities).
const (
	ToolListKinds       = "list-kinds"
	ToolDescribeKind    = "describe-kind"
	ToolResourceManager = "resource-manager"
	ToolKubectlRaw      = "kubectl-raw"
)

// GenericTools are the always-available generic tools (设计 §3.3.1 generic 层).
var GenericTools = []string{ToolListKinds, ToolDescribeKind, ToolResourceManager, ToolKubectlRaw}

// CRDSchema is one discovered CRD's schema (design §3.3.1: 平台启动读全部 CRD
// schema 缓存; list-kinds / describe-kind 动态发现即用).
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

// Catalog is the platform's capability catalog: discovered CRD schemas plus
// the registered Capability CRs. It answers "Agent 能用什么" (设计 §3.1).
type Catalog struct {
	discovery discovery.DiscoveryInterface
	dynamic   dynamic.Interface
	config    *rest.Config

	// schemas is the cached CRD schema table (group/kind → schema).
	schemas map[string]*CRDSchema
}

// NewCatalog builds a Catalog over the given REST config. The CRD schema table
// is lazily loaded (design §4.2.1: 平台启动读全部 CRD schema 缓存).
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

// Refresh re-discovers the CRD schema table (generic 工具集变更治理: CRD 增删改
// → 工具集自动变; 设计 §3.3.1 工具集变更治理).
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

// Schemas returns the current CRD schema table (group/kind → schema), sorted
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
		group = "assistant.suanova.io"
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

// ValidateCapability validates a Capability registration (设计 §3.3.1:
// target 指向的 CRD 不存在 / 无 schema → 登记校验 fail-fast).
func (c *Catalog) ValidateCapability(cap *v1alpha1.Capability) error {
	switch cap.Spec.Type {
	case v1alpha1.CapabilityAtomic:
		if !cap.Spec.Override {
			return fmt.Errorf("atomic capability %s must set override=true", cap.Name)
		}
		if cap.Spec.Target == nil {
			return fmt.Errorf("atomic capability %s must set spec.target (bound CRD)", cap.Name)
		}
		schema := c.SchemaFor(cap.Spec.Target.Group, cap.Spec.Target.Kind)
		if schema == nil {
			return fmt.Errorf("atomic capability %s: target CRD %s/%s not found in cluster (fail-fast)",
				cap.Name, cap.Spec.Target.Group, cap.Spec.Target.Kind)
		}
		if len(schema.OpenAPI) == 0 && cap.Spec.Target.Group == "" {
			return fmt.Errorf("atomic capability %s: target CRD %s has no schema", cap.Name, cap.Spec.Target.Kind)
		}
	case v1alpha1.CapabilityDomain:
		if cap.Spec.Instructions == "" && cap.Spec.ContentRef == "" {
			return fmt.Errorf("domain capability %s must set instructions or contentRef", cap.Name)
		}
	default:
		return fmt.Errorf("capability %s: unknown type %q", cap.Name, cap.Spec.Type)
	}
	return nil
}

// ToolSetForAgent computes the effective tool set for an Agent definition:
// generic tools are always available; the agent's tools[] references
// Capabilities (atomic + domain) whose visibility (spec.agents[]) admits the
// agent (设计 §3.3.1: Capability.agents[] 与 RBAC 共同决定可见子集).
func ToolSetForAgent(agent *v1alpha1.Agent, caps []v1alpha1.Capability) []string {
	set := map[string]bool{}
	for _, t := range GenericTools {
		set[t] = true
	}
	for _, ref := range agent.Spec.Tools {
		for _, cap := range caps {
			if cap.Name != ref {
				continue
			}
			// Visibility: empty agents[] = visible to all.
			if len(cap.Spec.Agents) > 0 && !contains(cap.Spec.Agents, agent.Name) {
				continue
			}
			set[ref] = true
			for _, u := range cap.Spec.Uses {
				set[u] = true
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CRDExists reports whether a CRD (group/kind) is present (for Capability
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

// Create applies one instance of a CRD kind (write path; phase-one 直放).
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
