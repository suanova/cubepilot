package skill

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestToolSetForAgent verifies the effective tool set computation
// (design §3.3.1: generic tools are available by default; each referenced
// Skill contributes its name).
func TestToolSetForAgent(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"},
		Spec: v1alpha1.AgentTemplateSpec{
			Skills: []string{"harbor", "cluster-inspection"},
		},
	}
	skills := []v1alpha1.Skill{
		{ObjectMeta: metav1.ObjectMeta{Name: "harbor"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cluster-inspection"}},
	}
	got := ToolSetForAgent(agent, skills)
	want := map[string]bool{
		"list-kinds": true, "describe-kind": true, "resource-manager": true,
		"kubectl-raw": true, "harbor": true, "cluster-inspection": true,
	}
	if len(got) != len(want) {
		t.Fatalf("tool set size = %d, want %d (%v)", len(got), len(want), got)
	}
	for _, tool := range got {
		if !want[tool] {
			t.Errorf("unexpected tool %q in set %v", tool, got)
		}
	}
}

// TestValidateSkill verifies the registration validation rules (design
// §3.4: source discriminant mirrors the CEL rules; phase 1 = Platform only).
func TestValidateSkill(t *testing.T) {
	c := &Catalog{}

	ok := &v1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor"},
		Spec: v1alpha1.SkillSpec{
			DisplayName: "Harbor",
			Visibility:  v1alpha1.SkillVisibilityPlatform,
			Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath, Path: "skills/harbor/v1.tar.gz"},
		},
	}
	if err := c.ValidateSkill(ok); err != nil {
		t.Errorf("valid skill rejected: %v", err)
	}

	// type=Path without path -> reject.
	noPath := ok.DeepCopy()
	noPath.Spec.Source.Path = ""
	if err := c.ValidateSkill(noPath); err == nil {
		t.Error("type=Path without path accepted")
	}

	// type=Path with s3 -> reject (mutually exclusive).
	withS3 := ok.DeepCopy()
	withS3.Spec.Source.S3 = &v1alpha1.SkillS3Source{Bucket: "b", Key: "k"}
	if err := c.ValidateSkill(withS3); err == nil {
		t.Error("type=Path with s3 accepted")
	}

	// visibility:User -> reject in phase 1.
	userVis := ok.DeepCopy()
	userVis.Spec.Visibility = v1alpha1.SkillVisibilityUser
	if err := c.ValidateSkill(userVis); err == nil {
		t.Error("visibility:User accepted in phase 1")
	}

	// source.type=S3 -> reject in phase 1.
	s3 := ok.DeepCopy()
	s3.Spec.Source = v1alpha1.SkillSource{Type: v1alpha1.SkillSourceS3}
	if err := c.ValidateSkill(s3); err == nil {
		t.Error("source.type=S3 accepted in phase 1")
	}
}

// TestFindKind verifies case-insensitive kind resolution.
func TestFindKind(t *testing.T) {
	c := &Catalog{schemas: map[string]*CRDSchema{
		"dev.suanova.io/DevEnvironment": {Kind: "DevEnvironment", Plural: "devenvironments"},
	}}
	if got := c.FindKind("devenvironment"); got == nil || got.Kind != "DevEnvironment" {
		t.Errorf("FindKind(devenvironment) = %v, want DevEnvironment", got)
	}
	if got := c.FindKind("nope"); got != nil {
		t.Errorf("FindKind(nope) = %v, want nil", got)
	}
}
