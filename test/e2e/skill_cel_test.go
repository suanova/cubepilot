package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// Skill CEL validation: the deployed skills CRD carries x-kubernetes-
// validations on spec.source, so the API server rejects source combinations
// that violate the Path/S3 discriminant (design §3.4).
var _ = Describe("Skill CEL validation", func() {
	ctx := context.Background()

	It("rejects source.type=Path with s3 set", func() {
		bad := &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-skill-path-s3"},
			Spec: v1alpha1.SkillSpec{
				DisplayName: "Bad",
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source: v1alpha1.SkillSource{
					Type: v1alpha1.SkillSourcePath,
					Path: "skills/x/v1.tar.gz", // valid path, but s3 is forbidden for Path
					S3:   &v1alpha1.SkillS3Source{Bucket: "b", Key: "k"},
				},
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "type=Path with s3 set must be rejected by CEL")
	})

	It("rejects source.type=Path without path", func() {
		bad := &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-skill-path-missing"},
			Spec: v1alpha1.SkillSpec{
				DisplayName: "Bad",
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath},
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "type=Path without path must be rejected by CEL")
	})

	It("rejects source.type=S3 with path set", func() {
		bad := &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-skill-s3"},
			Spec: v1alpha1.SkillSpec{
				DisplayName: "Bad",
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source: v1alpha1.SkillSource{
					Type: v1alpha1.SkillSourceS3,
					S3:   &v1alpha1.SkillS3Source{Bucket: "b", Key: "k"}, // valid s3, but path is forbidden for S3
					Path: "skills/x/v1.tar.gz",
				},
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "type=S3 with path set must be rejected by CEL")
	})

	It("accepts a valid source.type=Path skill", func() {
		good := &v1alpha1.Skill{
			ObjectMeta: metav1.ObjectMeta{Name: "good-skill-path"},
			Spec: v1alpha1.SkillSpec{
				DisplayName: "Good",
				Visibility:  v1alpha1.SkillVisibilityPlatform,
				Source:      v1alpha1.SkillSource{Type: v1alpha1.SkillSourcePath, Path: "skills/good/v1.tar.gz"},
			},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "a valid Path skill must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})
})
