package config

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

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
	t.Setenv("CUBEPILOT_DEFAULT_USER", "")
	t.Setenv("CUBEPILOT_LLM_ENDPOINT", "")
	t.Setenv("CUBEPILOT_LLM_MODEL", "")
	cfg := Load()
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

// TestLoadAgentImagePullPolicy verifies the env override reaches the config.
func TestLoadAgentImagePullPolicy(t *testing.T) {
	t.Setenv("CUBEPILOT_AGENT_IMAGE_PULL_POLICY", "Always")
	cfg := Load()
	if cfg.AgentImagePullPolicy != "Always" {
		t.Fatalf("AgentImagePullPolicy = %q, want Always", cfg.AgentImagePullPolicy)
	}
}

// TestLoadAgentDataStorageClass verifies the env override reaches the config.
func TestLoadAgentDataStorageClass(t *testing.T) {
	t.Setenv("CUBEPILOT_AGENT_DATA_STORAGE_CLASS", "ceph-rbd-ephemeral")
	cfg := Load()
	if cfg.AgentDataStorageClass != "ceph-rbd-ephemeral" {
		t.Fatalf("AgentDataStorageClass = %q, want ceph-rbd-ephemeral", cfg.AgentDataStorageClass)
	}
}

// TestLoadAgentImagePullPolicyDefault pins the kind-friendly default: local
// builds are side-loaded into the cluster, so an agent Pod must not go to the
// registry for the image.
func TestLoadAgentImagePullPolicyDefault(t *testing.T) {
	t.Setenv("CUBEPILOT_AGENT_IMAGE_PULL_POLICY", "")
	cfg := Load()
	if cfg.AgentImagePullPolicy != "IfNotPresent" {
		t.Fatalf("AgentImagePullPolicy = %q, want IfNotPresent", cfg.AgentImagePullPolicy)
	}
}

// TestLoadAgentScheduling verifies the JSON values the chart passes reach the
// config that schedules the agent Pods.
func TestLoadAgentScheduling(t *testing.T) {
	t.Setenv("CUBEPILOT_AGENT_NODE_SELECTOR", `{"kubernetes.io/os":"linux"}`)
	t.Setenv("CUBEPILOT_AGENT_TOLERATIONS",
		`[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}]`)
	cfg := Load()
	if len(cfg.AgentNodeSelector) != 1 || cfg.AgentNodeSelector["kubernetes.io/os"] != "linux" {
		t.Fatalf("AgentNodeSelector = %v, want kubernetes.io/os=linux", cfg.AgentNodeSelector)
	}
	if len(cfg.AgentTolerations) != 1 ||
		cfg.AgentTolerations[0].Key != "node-role.kubernetes.io/control-plane" ||
		cfg.AgentTolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("AgentTolerations = %v, want the control-plane NoSchedule toleration", cfg.AgentTolerations)
	}
}

// TestLoadAgentSchedulingMalformed pins the fallback: a bad env value leaves
// the Pods on the cluster scheduler instead of failing operator startup.
func TestLoadAgentSchedulingMalformed(t *testing.T) {
	t.Setenv("CUBEPILOT_AGENT_NODE_SELECTOR", "not-json")
	t.Setenv("CUBEPILOT_AGENT_TOLERATIONS", "not-json")
	cfg := Load()
	if cfg.AgentNodeSelector != nil {
		t.Fatalf("AgentNodeSelector = %v, want nil", cfg.AgentNodeSelector)
	}
	if cfg.AgentTolerations != nil {
		t.Fatalf("AgentTolerations = %v, want nil", cfg.AgentTolerations)
	}
}

// TestLoadAgentSchedulingDefault verifies an unset env leaves both empty.
func TestLoadAgentSchedulingDefault(t *testing.T) {
	t.Setenv("CUBEPILOT_AGENT_NODE_SELECTOR", "")
	t.Setenv("CUBEPILOT_AGENT_TOLERATIONS", "")
	cfg := Load()
	if cfg.AgentNodeSelector != nil || cfg.AgentTolerations != nil {
		t.Fatalf("scheduling should be empty, got %v / %v", cfg.AgentNodeSelector, cfg.AgentTolerations)
	}
}
