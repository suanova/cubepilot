package skill

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinSkillNames verifies the embedded presets are enumerated.
func TestBuiltinSkillNames(t *testing.T) {
	names := BuiltinSkillNames()
	want := map[string]bool{
		"cluster-inspection": true,
		"kubectl-platform":   true,
	}
	if len(names) != len(want) {
		t.Fatalf("builtin names = %v, want %d", names, len(want))
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected builtin %q", n)
		}
	}
}

// TestPackBuiltinSkill verifies each preset packs to a valid gzip tar that
// extracts to SKILL.md, with non-empty displayName/description metadata.
func TestPackBuiltinSkill(t *testing.T) {
	for _, name := range BuiltinSkillNames() {
		tar, displayName, description, err := PackBuiltinSkill(name)
		if err != nil {
			t.Fatalf("PackBuiltinSkill(%q): %v", name, err)
		}
		if displayName == "" || description == "" {
			t.Errorf("PackBuiltinSkill(%q): displayName=%q description=%q, want non-empty", name, displayName, description)
		}
		dest := t.TempDir()
		if err := ExtractTar(bytes.NewReader(tar), dest); err != nil {
			t.Errorf("PackBuiltinSkill(%q) tar not extractable: %v", name, err)
			continue
		}
		if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
			t.Errorf("PackBuiltinSkill(%q) has no SKILL.md: %v", name, err)
		}
	}
}
