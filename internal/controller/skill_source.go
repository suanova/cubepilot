package controller

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/skill"
)

// skillsFS embeds the preset skill directories. Each skills/<name>/ is the
// single source of truth for one preset skill: the bootstrap seeds it into
// the skill repository as a versioned tar and registers a Skill CRD
// referencing the tar (design §3.4: content lives in the repository; the CRD
// registers where + which version).
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

// SeedBuiltinSkills packs the embedded preset skill directories into the
// repository as versioned tars (skills/<name>/v1.tar.gz) and returns the
// Skill CRDs referencing them (source.path + source.sha256 backfilled).
// Idempotent: an existing version whose content matches is not rewritten; a
// content change writes the next version.
func SeedBuiltinSkills(ctx context.Context, repo skill.Repository) ([]*v1alpha1.Skill, error) {
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
		sub, err := fs.Sub(skillsFS, "skills/"+name)
		if err != nil {
			return nil, err
		}
		ver, sha, err := seedVersion(ctx, repo, name, sub)
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
				DisplayName: skillTitle(body),
				Description: meta.Description,
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source: v1alpha1.SkillSource{
					Type:   v1alpha1.SkillSourcePath,
					Path:   fmt.Sprintf("skills/%s/%s.tar.gz", name, ver),
					Sha256: sha,
				},
			},
			Status: v1alpha1.SkillStatus{Phase: v1alpha1.SkillPhaseAvailable},
		})
	}
	return out, nil
}

// seedVersion writes the skill dir as skills/<name>/vN.tar.gz, bumping N when
// the packed content differs from an existing version; returns the version
// label and the tar sha256.
func seedVersion(ctx context.Context, repo skill.Repository, name string, sub fs.FS) (string, string, error) {
	packed, err := skill.PackSha256(sub)
	if err != nil {
		return "", "", err
	}
	for v := 1; ; v++ {
		p := fmt.Sprintf("skills/%s/v%d.tar.gz", name, v)
		rc, err := repo.Open(ctx, p)
		if err != nil {
			sha, err := repo.Write(ctx, p, sub)
			return fmt.Sprintf("v%d", v), sha, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, rc); err != nil {
			rc.Close()
			return "", "", err
		}
		rc.Close()
		if hex.EncodeToString(h.Sum(nil)) == packed {
			return fmt.Sprintf("v%d", v), packed, nil
		}
	}
}
