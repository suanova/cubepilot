package config

import "testing"

func TestLoadSkillsDir(t *testing.T) {
	t.Setenv("CUBEPILOT_SKILLS_DIR", "/mnt/skills")
	cfg := Load()
	if cfg.SkillsDir != "/mnt/skills" {
		t.Fatalf("SkillsDir = %q, want /mnt/skills", cfg.SkillsDir)
	}
}

func TestLoadSkillsDirDefault(t *testing.T) {
	t.Setenv("CUBEPILOT_SKILLS_DIR", "")
	cfg := Load()
	if cfg.SkillsDir == "" {
		t.Fatal("SkillsDir should have a default")
	}
}
