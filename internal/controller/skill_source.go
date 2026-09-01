package controller

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/skill"
)

// skillsFS embeds the preset skill directories. Each skills/<name>/ is the
// single source of truth for one preset skill: the bootstrap publishes it to
// the skill API (which owns the repository + Skill CRD registration, design
// §3.4).
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

// PublishBuiltinSkills publishes the embedded preset skills to the skill API
// (POST /internal/skills/{name}/publish, gzip tar body + metadata) and returns
// the resulting Skill CRs. The API owns the repository and the Skill CRD
// registration; the operator only supplies the content (design §3.4 publish
// flow). Idempotent: re-publishing identical content returns the same version.
func PublishBuiltinSkills(ctx context.Context, apiURL string, httpClient *http.Client) ([]*v1alpha1.Skill, error) {
	names, err := presetSkillNames()
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	var out []*v1alpha1.Skill
	for _, name := range names {
		meta, body, err := loadSkill(name)
		if err != nil {
			return nil, err
		}
		sub, err := fs.Sub(skillsFS, "skills/"+name)
		if err != nil {
			return nil, err
		}
		tar, err := skill.Pack(sub)
		if err != nil {
			return nil, err
		}
		u := fmt.Sprintf("%s/internal/skills/%s/publish?displayName=%s&description=%s&builtin=true",
			strings.TrimRight(apiURL, "/"), url.PathEscape(name),
			url.QueryEscape(skillTitle(body)), url.QueryEscape(meta.Description))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(tar))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/gzip")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("publish %s: %w", name, err)
		}
		payload, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read publish %s: %w", name, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("publish %s: %d: %s", name, resp.StatusCode, strings.TrimSpace(string(payload)))
		}
		var skillCR v1alpha1.Skill
		if err := json.Unmarshal(payload, &skillCR); err != nil {
			return nil, fmt.Errorf("decode publish %s: %w", name, err)
		}
		out = append(out, &skillCR)
	}
	return out, nil
}
