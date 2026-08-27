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

// skillsFS embeds the preset skill SKILL.md files. Each
// skills/<name>/SKILL.md is the single source of truth for one preset
// domain skill: the bootstrap renders it into a Skill CRD, and the
// resolver/supervisor renders the CRD back into a runtime skill (design
// §3.4: Skill is the platform truth, the runtime skill is its OpenClaw
// presentation).
//
//go:embed skills/*/SKILL.md
var skillsFS embed.FS

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
// Skill title); returns "" when the body has none.
func skillTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

// loadSkill reads one embedded SKILL.md by skill name.
func loadSkill(name string) (skillMeta, string, error) {
	path := "skills/" + name + "/SKILL.md"
	raw, err := skillsFS.ReadFile(path)
	if err != nil {
		return skillMeta{}, "", err
	}
	return parseSKILL(string(raw))
}

// presetSkillNames returns the embedded skill names, sorted for
// deterministic bootstrap order.
func presetSkillNames() ([]string, error) {
	entries, err := fs.ReadDir(skillsFS, "skills")
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

// BuiltinSkillDefinitions returns the preset domain skills
// generated from the embedded SKILL.md files. The Skill CRD carries the
// platform semantics (title/description/instructions); the resolver renders
// the CRD back into the runtime skill -- one source, two presentations.
func BuiltinSkillDefinitions() ([]*v1alpha1.Skill, error) {
	names, err := presetSkillNames()
	if err != nil {
		return nil, err
	}
	var out []*v1alpha1.Skill
	for _, name := range names {
		meta, body, err := loadSkill(name)
		if err != nil {
			return nil, err
		}
		out = append(out, &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app.kubernetes.io/part-of": "cubepilot",
					"cubepilot/builtin":         "true",
				},
			},
			Spec: v1alpha1.SkillSpec{
				Type:         v1alpha1.SkillDomain,
				Title:        skillTitle(body),
				Description:  meta.Description,
				OwnerModule:  "Platform",
				Instructions: body,
			},
		})
	}
	return out, nil
}
