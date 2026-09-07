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

func TestLoadDefaultsSingleAdmin(t *testing.T) {
	t.Setenv("CUBEPILOT_USERS", "")
	t.Setenv("CUBEPILOT_DEFAULT_USER", "")
	cfg := Load()
	if len(cfg.Users) != 1 || cfg.Users[0] != "admin" {
		t.Fatalf("Users = %v, want [admin]", cfg.Users)
	}
	if cfg.DefaultUser != "admin" {
		t.Fatalf("DefaultUser = %q, want admin", cfg.DefaultUser)
	}
}
