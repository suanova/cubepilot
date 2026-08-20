package controller

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// capabilitiesFS embeds the preset capability SKILL.md files. Each
// capabilities/<name>/SKILL.md is the single source of truth for one preset
// domain capability: the bootstrap renders it into a Capability CRD, and the
// resolver/supervisor renders the CRD back into a runtime skill (design
// §3.3.1: Capability is the platform truth, the runtime skill is its OpenClaw
// presentation).
//
//go:embed capabilities/*/SKILL.md
var capabilitiesFS embed.FS

// skillMeta is the YAML frontmatter of a SKILL.md.
type skillMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseSKILL splits a SKILL.md into frontmatter metadata and body.
func parseSKILL(raw string) (skillMeta, string, error) {
	var meta skillMeta
	rest := strings.TrimPrefix(raw, "---\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return meta, "", fmt.Errorf("missing closing --- frontmatter")
	}
	fm := rest[:idx]
	body := strings.TrimPrefix(rest[idx+len("\n---"):], "\n")
	body = strings.TrimSpace(body)
	if err := yaml.Unmarshal([]byte(fm), &meta); err != nil {
		return meta, "", fmt.Errorf("parse frontmatter: %w", err)
	}
	if meta.Name == "" {
		return meta, "", fmt.Errorf("frontmatter name missing")
	}
	return meta, body, nil
}

// skillTitle extracts the first H1 heading from a skill body (used as the
// Capability title); returns "" when the body has none.
func skillTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

// loadSkill reads one embedded SKILL.md by capability name.
func loadSkill(name string) (skillMeta, string, error) {
	path := "capabilities/" + name + "/SKILL.md"
	raw, err := capabilitiesFS.ReadFile(path)
	if err != nil {
		return skillMeta{}, "", err
	}
	return parseSKILL(string(raw))
}

// presetCapabilityNames returns the embedded skill names, sorted for
// deterministic bootstrap order.
func presetCapabilityNames() ([]string, error) {
	entries, err := fs.ReadDir(capabilitiesFS, "capabilities")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// BuiltinCapabilityDefinitions returns the preset domain capabilities
// generated from the embedded SKILL.md files. The Capability CRD carries the
// platform semantics (title/description/instructions); the resolver renders
// the CRD back into the runtime skill — one source, two presentations.
func BuiltinCapabilityDefinitions() ([]*v1alpha1.Capability, error) {
	names, err := presetCapabilityNames()
	if err != nil {
		return nil, err
	}
	var out []*v1alpha1.Capability
	for _, name := range names {
		meta, body, err := loadSkill(name)
		if err != nil {
			return nil, err
		}
		out = append(out, &v1alpha1.Capability{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "cubepilot",
					"cubepilot/builtin":         "true",
				},
			},
			Spec: v1alpha1.CapabilitySpec{
				Type:         v1alpha1.CapabilityDomain,
				Title:        skillTitle(body),
				Description:  meta.Description,
				OwnerModule:  "platform",
				Instructions: body,
			},
		})
	}
	return out, nil
}
