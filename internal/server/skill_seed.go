package server

import (
	"context"
	"log"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/skill"
)

// SeedBuiltinSkills registers the embedded preset skills into the repository +
// Skill CRDs at startup (the API owns the skill lifecycle: seed + serve +
// publish). Idempotent; retries every 30s until success so a not-yet-ready
// repository / k8s client converges. Blocks until ctx is done or seeding
// succeeds, so callers launch it in a goroutine.
func (s *Server) SeedBuiltinSkills(ctx context.Context) {
	for {
		err := s.seedBuiltinSkillsOnce(ctx)
		if err == nil {
			log.Printf("skill seed: %d builtin skills registered", len(skill.BuiltinSkillNames()))
			return
		}
		log.Printf("skill seed: %v (retrying)", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// seedBuiltinSkillsOnce publishes every embedded preset through publishSkill
// (the same path the HTTP endpoint and #23's Portal upload use).
func (s *Server) seedBuiltinSkillsOnce(ctx context.Context) error {
	for _, name := range skill.BuiltinSkillNames() {
		tar, displayName, description, err := skill.PackBuiltinSkill(name)
		if err != nil {
			return err
		}
		if _, err := s.publishSkill(ctx, name, displayName, description, v1alpha1.SkillVisibilityPlatform, tar, true); err != nil {
			return err
		}
	}
	return nil
}
