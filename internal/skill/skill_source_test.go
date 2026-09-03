package skill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltinSkillNames verifies the embedded presets are enumerated.
func TestBuiltinSkillNames(t *testing.T) {
	names := BuiltinSkillNames()
	want := map[string]bool{
		"cluster-inspection": true,
		"kubectl-platform":   true,
		"cubestack-platform": true,
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

// TestCubestackPlatformSkillPacksReference verifies the packed tar of the
// cubestack-platform skill carries its generated crd-reference.md alongside
// SKILL.md, so a revert of the go:embed pattern (all:skills/*) cannot silently
// ship the skill without its schema map (issue #98).
func TestCubestackPlatformSkillPacksReference(t *testing.T) {
	tar, _, _, err := PackBuiltinSkill("cubestack-platform")
	if err != nil {
		t.Fatalf("PackBuiltinSkill: %v", err)
	}
	dest := t.TempDir()
	if err := ExtractTar(bytes.NewReader(tar), dest); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	for _, want := range []string{"SKILL.md", "crd-reference.md"} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("packed skill missing %s: %v", want, err)
		}
	}
}

// TestKubectlPlatformSkillTeachesDiscovery verifies the built-in generic
// skill teaches schema discovery (design §5.3), so the agent can operate any
// CRD without a per-CRD skill (issue #86), and that discovery uses the
// platform kubeconfig while real operations stay on the user identity (issue
// #19 dual kubeconfig).
func TestKubectlPlatformSkillTeachesDiscovery(t *testing.T) {
	raw, err := skillsFS.ReadFile("skills/kubectl-platform/SKILL.md")
	if err != nil {
		t.Fatalf("read kubectl-platform skill: %v", err)
	}
	text := string(raw)
	// Identity routing: the discovery steps (api-resources and schema reads)
	// must use the platform kubeconfig explicitly; writes stay on the user
	// identity. Assert the full command lines, not independent tokens.
	for _, want := range []string{
		"--kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG api-resources",
		"--kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG explain",
		"--dry-run=server",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("kubectl-platform SKILL.md should mention %q (schema discovery / dual kubeconfig)", want)
		}
	}
	if strings.Contains(text, "resources.requests") {
		t.Errorf("kubectl-platform SKILL.md example should use the flat resources shape (cpu/memory), not requests/limits")
	}
}
