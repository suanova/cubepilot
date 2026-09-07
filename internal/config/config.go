// Package config loads CubePilot configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultLLMEndpoint is the default OpenAI-compatible base URL of the platform
// default LLM (the builtin agent-for-cloud model).
const DefaultLLMEndpoint = "https://api.deepseek.com"

// DefaultLLMModel is the default model name (backend model id) of the platform
// default LLM (the builtin agent-for-cloud model).
const DefaultLLMModel = "deepseek-v4-flash"

// Config holds all runtime configuration for the assistant service + instance manager.
type Config struct {
	// Listen is the HTTP listen address for the assistant service (Portal + API).
	Listen string

	// Namespace is the Kubernetes namespace where per-user agent Pods live.
	Namespace string

	// AgentImage is the per-user OpenClaw agent image (with kubectl + skill catalog).
	AgentImage string

	// GatewayToken is the bearer token used to authenticate against each agent
	// gateway (mirrors gateway.auth.token in the injected openclaw.json).
	GatewayToken string

	// LLMEndpoint is the OpenAI-compatible base URL of the platform default LLM
	// (the builtin AgentTemplate's model endpoint). Empty by default: no default
	// model is seeded until an endpoint AND model are configured. The apiKey
	// lives in the cubepilot-llm Secret, never here.
	LLMEndpoint string

	// LLMModel is the model name (backend model id) of the platform default LLM
	// (the builtin AgentTemplate's model name). Empty by default; must match the
	// configured LLM endpoint's allow-list when set.
	LLMModel string

	// IdleTTL is how long an agent instance may sit idle before the manager
	// reclaims it. Only effective when ReclaimEnabled is true (design doc §5.2:
	// resident is the default policy, idle reclaim is a configurable policy).
	IdleTTL time.Duration

	// ReclaimEnabled gates idle reclaim (design doc §5.2 / FR-M2-002). Default
	// false = instances stay resident once started; enabling it switches the
	// lifecycle to on-demand start + idle reclaim. (Resident is the default
	// policy; idle reclaim is a configurable policy.)
	ReclaimEnabled bool

	// Replicas is the desired Instance Manager / scheduler replica count used
	// for leader election. 1 = no election (single replica).
	Replicas int

	// GCWindow is the retention window for per-user data directories
	// (design doc §5.1: 48~72h sliding window). Session/transcript files older
	// than this are pruned by the manager's GC pass.
	GCWindow time.Duration

	// GCWatermark is the PVC usage ratio that triggers an aggressive GC + log
	// warning (design doc §10: watermark >70% triggers cleanup/alert).
	GCWatermark float64

	// Users is the set of operator identities, one independent instance each.
	// Defaults to a single 'admin' (per-user SA + kubeconfig identity is minted
	// for each at deploy time; new users cannot be added from the Portal).
	Users []string

	// DefaultUser is used when a request carries no explicit operator identity.
	DefaultUser string

	// AgentPort is the port the OpenClaw gateway listens on inside each agent Pod.
	AgentPort int

	// DataDir is where platform metadata (tasks / reports / audit / agent config)
	// is persisted as JSON files -- backed by a PVC on the backend Pod.
	DataDir string

	// MetricsAddr is the bind address for the operator's controller-runtime
	// metrics endpoint ("0" disables).
	MetricsAddr string

	// ProbeAddr is the bind address for the operator's health/ready probes
	// ("0" disables).
	ProbeAddr string

	// SkillsDir is the skill repository root (shared volume). Owned by the API
	// server (publish + seed + serve); agent Pods never mount it (the
	// supervisor pulls over HTTP).
	SkillsDir string
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	cfg := Config{
		Listen:       getenv("CUBEPILOT_LISTEN", ":8080"),
		Namespace:    getenv("CUBEPILOT_NAMESPACE", "cubepilot"),
		AgentImage:   getenv("CUBEPILOT_AGENT_IMAGE", "harbor.isuanova.com/suanova/cubepilot-openclaw:local"),
		GatewayToken: os.Getenv("CUBEPILOT_GATEWAY_TOKEN"),
		// Empty by default: no platform default model is seeded unless an
		// endpoint + model are configured (the builtin template is then created
		// model-less and LLMs are added from the Portal).
		LLMEndpoint:    getenv("CUBEPILOT_LLM_ENDPOINT", ""),
		LLMModel:       getenv("CUBEPILOT_LLM_MODEL", ""),
		IdleTTL:        getDuration("CUBEPILOT_IDLE_TTL", 30*time.Minute),
		ReclaimEnabled: getBool("CUBEPILOT_RECLAIM", false),
		Replicas:       getInt("CUBEPILOT_REPLICAS", 1),
		GCWindow:       getDuration("CUBEPILOT_GC_WINDOW", 72*time.Hour),
		GCWatermark:    getFloat("CUBEPILOT_GC_WATERMARK", 0.7),
		DefaultUser:    getenv("CUBEPILOT_DEFAULT_USER", "admin"),
		AgentPort:      getInt("CUBEPILOT_AGENT_PORT", 18789),
		DataDir:        getenv("CUBEPILOT_DATA_DIR", "/opt/cubepilot/data"),
		MetricsAddr:    getenv("CUBEPILOT_METRICS_ADDR", "0"),
		ProbeAddr:      getenv("CUBEPILOT_PROBE_ADDR", "0"),
		SkillsDir:      getenv("CUBEPILOT_SKILLS_DIR", "/var/lib/cubepilot/skills"),
	}
	users := getenv("CUBEPILOT_USERS", "admin")
	for _, u := range strings.Split(users, ",") {
		if u = strings.TrimSpace(u); u != "" {
			cfg.Users = append(cfg.Users, u)
		}
	}
	return cfg
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return n
}

func getFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return n
}
