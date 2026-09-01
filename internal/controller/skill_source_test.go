package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/skill"
)

// TestPublishBuiltinSkills verifies the operator publishes each embedded
// preset to the skill API's publish endpoint: one POST per preset, gzip tar
// body, displayName/description/builtin metadata, and the returned Skill CRs.
func TestPublishBuiltinSkills(t *testing.T) {
	var calls []string
	var bodies [][]byte
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/skills/{name}/publish", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		calls = append(calls, name)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		bodies = append(bodies, body)
		if r.URL.Query().Get("builtin") != "true" {
			t.Errorf("publish %s: builtin not set", name)
		}
		_ = json.NewEncoder(w).Encode(&v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"cubepilot/builtin": "true"}},
			Spec: v1alpha1.SkillSpec{
				DisplayName: r.URL.Query().Get("displayName"),
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath, Path: "skills/" + name + "/v1.tar.gz", Sha256: "x"},
			},
			Status: v1alpha1.SkillStatus{Phase: v1alpha1.SkillPhaseAvailable},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	skills, err := PublishBuiltinSkills(t.Context(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("PublishBuiltinSkills: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("no skills published")
	}
	if len(calls) != len(skills) {
		t.Fatalf("publish calls = %d, skills = %d", len(calls), len(skills))
	}
	for i, s := range skills {
		if s.Labels["cubepilot/builtin"] != "true" {
			t.Errorf("skill %s: builtin label missing", s.Name)
		}
		if s.Name != calls[i] {
			t.Errorf("skill %s != call %s", s.Name, calls[i])
		}
		// Each body must be a valid gzip tar that extracts to SKILL.md.
		dir := t.TempDir()
		if err := skill.ExtractTar(bytes.NewReader(bodies[i]), dir); err != nil {
			t.Errorf("publish %s body is not a valid gzip tar: %v", s.Name, err)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
			t.Errorf("publish %s body has no SKILL.md: %v", s.Name, err)
		}
	}
}
