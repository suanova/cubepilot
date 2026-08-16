// Package config loads CubePilot PoC configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the assistant service + instance manager.
type Config struct {
	// Listen is the HTTP listen address for the assistant service (Portal + API).
	Listen string

	// Namespace is the Kubernetes namespace where per-user agent Pods live.
	Namespace string

	// AgentImage is the per-user OpenClaw agent image (with kubectl + capability catalog).
	AgentImage string

	// GatewayToken is the bearer token used to authenticate against each agent
	// gateway (mirrors gateway.auth.token in the injected openclaw.json).
	GatewayToken string

	// IdleTTL is how long an agent instance may sit idle before the manager reclaims it.
	IdleTTL time.Duration

	// Users is the set of demo operator identities, one independent instance each.
	Users []string

	// DefaultUser is used when a request carries no explicit operator identity.
	DefaultUser string

	// AgentPort is the port the OpenClaw gateway listens on inside each agent Pod.
	AgentPort int

	// DataDir is where PoC metadata (tasks / reports / audit / agent config)
	// is persisted as JSON files — backed by a PVC on the backend Pod.
	DataDir string
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	cfg := Config{
		Listen:       getenv("CUBEPILOT_LISTEN", ":8080"),
		Namespace:    getenv("CUBEPILOT_NAMESPACE", "cubepilot"),
		AgentImage:   getenv("CUBEPILOT_AGENT_IMAGE", "cubepilot-agent:local"),
		GatewayToken: os.Getenv("CUBEPILOT_GATEWAY_TOKEN"),
		IdleTTL:      getDuration("CUBEPILOT_IDLE_TTL", 30*time.Minute),
		DefaultUser:  getenv("CUBEPILOT_DEFAULT_USER", "zhang.wei"),
		AgentPort:    getInt("CUBEPILOT_AGENT_PORT", 18789),
		DataDir:      getenv("CUBEPILOT_DATA_DIR", "/opt/cubepilot/data"),
	}
	users := getenv("CUBEPILOT_USERS", "zhang.wei,li.ming")
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
