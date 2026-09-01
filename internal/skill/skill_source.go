package skill

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillsFS embeds the preset skill directories. Each skills/<name>/ is the
// single source of truth for one builtin skill (design §3.4): the API seeds it
// into the repository at startup, and the operator references the names.
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
// Skill displayName); returns "" when the body has none.
func skillTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

// BuiltinSkillNames returns the embedded preset skill names, sorted for a
// deterministic seed order.
func BuiltinSkillNames() []string {
	entries, err := fs.ReadDir(skillsFS, "skills")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// PackBuiltinSkill packs one embedded preset skill directory into a gzip tar
// and returns it with its market metadata (displayName from the first H1,
// description from the frontmatter). Used by the API's startup seed.
func PackBuiltinSkill(name string) (tar []byte, displayName, description string, err error) {
	raw, err := skillsFS.ReadFile("skills/" + name + "/SKILL.md")
	if err != nil {
		return nil, "", "", err
	}
	meta, body, err := parseSKILL(string(raw))
	if err != nil {
		return nil, "", "", err
	}
	sub, err := fs.Sub(skillsFS, "skills/"+name)
	if err != nil {
		return nil, "", "", err
	}
	tar, err = Pack(sub)
	if err != nil {
		return nil, "", "", err
	}
	return tar, skillTitle(body), meta.Description, nil
}
