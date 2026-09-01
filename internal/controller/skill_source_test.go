package controller

import (
	"testing"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/skill"
)

func TestSeedBuiltinSkills(t *testing.T) {
	repo := &skill.PathRepository{Root: t.TempDir()}
	skills, err := SeedBuiltinSkills(t.Context(), repo)
	if err != nil {
		t.Fatalf("SeedBuiltinSkills: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("no preset skills seeded")
	}
	for _, s := range skills {
		if s.Spec.Visibility != v1alpha1.SkillVisibilityPlatform {
			t.Errorf("skill %s: visibility = %q, want Platform", s.Name, s.Spec.Visibility)
		}
		if s.Spec.Source.Type != v1alpha1.SkillSourcePath || s.Spec.Source.Path == "" {
			t.Errorf("skill %s: source = %+v, want Path with a path", s.Name, s.Spec.Source)
		}
		if s.Spec.Source.Sha256 == "" {
			t.Errorf("skill %s: sha256 not backfilled", s.Name)
		}
		// Idempotent: re-seeding returns the same versioned path.
		again, err := SeedBuiltinSkills(t.Context(), repo)
		if err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		if len(again) != len(skills) {
			t.Fatalf("re-seed count = %d, want %d", len(again), len(skills))
		}
		for i := range skills {
			if again[i].Spec.Source.Path != skills[i].Spec.Source.Path {
				t.Errorf("re-seed changed path %q -> %q", skills[i].Spec.Source.Path, again[i].Spec.Source.Path)
			}
		}
	}
}
