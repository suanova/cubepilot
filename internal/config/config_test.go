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
	t.Setenv("CUBEPILOT_LLM_ENDPOINT", "")
	t.Setenv("CUBEPILOT_LLM_MODEL", "")
	cfg := Load()
	if len(cfg.Users) != 1 || cfg.Users[0] != "admin" {
		t.Fatalf("Users = %v, want [admin]", cfg.Users)
	}
	if cfg.DefaultUser != "admin" {
		t.Fatalf("DefaultUser = %q, want admin", cfg.DefaultUser)
	}
	// No default LLM is assumed: the platform default model is only seeded when
	// an endpoint + model are explicitly configured.
	if cfg.LLMEndpoint != "" || cfg.LLMModel != "" {
		t.Fatalf("LLM endpoint/model should default empty, got %q / %q", cfg.LLMEndpoint, cfg.LLMModel)
	}
}

func TestLoadLLMDefaultsFromEnv(t *testing.T) {
	t.Setenv("CUBEPILOT_LLM_ENDPOINT", "https://llm.example.com/v1")
	t.Setenv("CUBEPILOT_LLM_MODEL", "my-model")
	cfg := Load()
	if cfg.LLMEndpoint != "https://llm.example.com/v1" {
		t.Fatalf("LLMEndpoint = %q, want env value", cfg.LLMEndpoint)
	}
	if cfg.LLMModel != "my-model" {
		t.Fatalf("LLMModel = %q, want env value", cfg.LLMModel)
	}
}
