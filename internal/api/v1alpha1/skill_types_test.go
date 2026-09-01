package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSkillRevisionHashesSource(t *testing.T) {
	s := &Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor"},
		Spec: SkillSpec{
			DisplayName: "Harbor",
			Visibility:  SkillVisibilityPlatform,
			Source:      SkillSource{Type: SkillSourcePath, Path: "skills/harbor/v1.tar.gz", Sha256: "abc"},
		},
	}
	if s.Revision() == "" {
		t.Fatal("empty revision")
	}
	changed := s.DeepCopy()
	changed.Spec.Source.Path = "skills/harbor/v2.tar.gz"
	if changed.Revision() == s.Revision() {
		t.Fatal("revision should change when source.path changes")
	}
}

func TestSkillSpecMarketplaceShape(t *testing.T) {
	s := &Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "harbor"},
		Spec: SkillSpec{
			DisplayName: "Harbor",
			Visibility:  SkillVisibilityPlatform,
			Source:      SkillSource{Type: SkillSourcePath, Path: "skills/harbor/v1.tar.gz"},
		},
		Status: SkillStatus{Phase: SkillPhaseAvailable},
	}
	_ = s
}
