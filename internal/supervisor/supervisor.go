// Package supervisor implements the agent-pod-side runtime supervisor: it
// pulls the resolved agent config from the platform internal API, renders
// domain skills into the OpenClaw workspace as skills, and manages the
// OpenClaw gateway process (graceful restart on config change -- the pod is
// never deleted, so sessions/PVC/IP survive). It is the "agent supervisor"
// of the final architecture: CRDs declare -> operator resolves -> supervisor
// renders -> OpenClaw executes.
//
// The gateway config (LLM providers / model allowlist) is rendered by the
// operator into the openclaw-config Secret and pulled by the supervisor from
// the internal API (GET /internal/gateway/config) into a writable path; when
// the content changes the gateway is gracefully restarted. The read-only
// Secret mount is used only as a cold-start seed. This is how providers are
// added/edited post-install without touching Pods or scripts/setup.sh.
package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/suanova/cubepilot/internal/resolver"
)

// Config carries the supervisor's own configuration (from env).
type Config struct {
	// APIURL is the platform internal API base URL (e.g.
	// http://cubepilot-api.cubepilot.svc:8080). The supervisor polls
	// {APIURL}/internal/agents/{User}/config for the resolved config.
	APIURL string
	// User is the instance owner whose config this supervisor serves.
	User string
	// Workspace is the OpenClaw workspace directory (skills land in
	// Workspace/skills/<name>/SKILL.md).
	Workspace string
	// GatewayCmd is the OpenClaw gateway command (argv[0] + args).
	GatewayCmd []string
	// PollInterval is how often the resolved config is re-fetched.
	PollInterval time.Duration
	// ConfigPath is the writable gateway config file (openclaw.json) the
	// supervisor renders from the internal API. The gateway reads it at startup
	// (OPENCLAW_CONFIG_PATH); when the pulled content changes the supervisor
	// restarts the gateway so the new config applies. Empty disables the pull.
	ConfigPath string
	// SeedPath is the read-only openclaw-config Secret mount used as a
	// cold-start fallback when the internal API is unreachable before the
	// gateway starts. Empty disables the fallback.
	SeedPath string
}

// LoadFromEnv builds a Config from the environment with sane defaults.
func LoadFromEnv() Config {
	return Config{
		APIURL:       getenv("CUBEPILOT_API_URL", "http://cubepilot-api.cubepilot.svc:8080"),
		User:         os.Getenv("CUBEPILOT_AGENT_USER"),
		Workspace:    getenv("CUBEPILOT_WORKSPACE", "/home/node/.openclaw/workspace"),
		GatewayCmd:   []string{"node", "dist/index.js", "gateway", "--bind", "lan", "--port", "18789"},
		PollInterval: 10 * time.Second,
		ConfigPath:   getenv("OPENCLAW_CONFIG_PATH", "/home/node/.openclaw/gateway/openclaw.json"),
		SeedPath:     getenv("CUBEPILOT_CONFIG_SEED", "/home/node/.openclaw/openclaw.json"),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Supervisor manages the OpenClaw gateway process and keeps the workspace
// skills in sync with the resolved agent config.
type Supervisor struct {
	cfg     Config
	http    *http.Client
	mu      sync.Mutex
	current string // last applied config revision

	cmdMu sync.Mutex
	cmd   *exec.Cmd

	// lastCfgHash is the sha256 of the gateway config file as last loaded (or
	// detected); configHashChanged compares against it. Only touched by Run.
	lastCfgHash string
}

// New returns a Supervisor for the given config.
func New(cfg Config) *Supervisor {
	return &Supervisor{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// Run is the supervisor main loop: apply the current config, start the
// gateway, then poll the resolved config and reload (render skills +
// graceful restart) on revision change.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.cfg.User == "" {
		return fmt.Errorf("CUBEPILOT_AGENT_USER is required")
	}
	// Apply the current config before the first start so the gateway boots
	// with the right skills already in place (no boot-time restart).
	if _, err := s.poll(ctx); err != nil {
		log.Printf("supervisor: initial config poll: %v (runtime defaults apply until next poll)", err)
	}
	// Ensure the writable gateway config exists before the gateway starts:
	// pull it from the internal API, falling back to the mounted Secret seed.
	if err := s.seedGatewayConfig(ctx); err != nil {
		log.Printf("supervisor: seed gateway config: %v (gateway starts with defaults)", err)
	}
	if err := s.startGateway(ctx); err != nil {
		return fmt.Errorf("start gateway: %w", err)
	}

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopGateway()
			return ctx.Err()
		case <-ticker.C:
			changed, err := s.poll(ctx)
			if err != nil {
				log.Printf("supervisor: poll: %v", err)
			}
			// Pull the rendered gateway config so provider/model allowlist
			// changes apply without waiting on the kubelet Secret-volume sync.
			gcChanged, gcerr := s.refreshGatewayConfig(ctx)
			if gcerr != nil {
				log.Printf("supervisor: refresh gateway config: %v", gcerr)
			}
			if gcChanged {
				log.Printf("supervisor: gateway config changed; restarting")
				changed = true
			}
			if changed {
				log.Printf("supervisor: restarting gateway for new config")
				if err := s.restartGateway(ctx); err != nil {
					log.Printf("supervisor: restart: %v", err)
				}
			}
		}
	}
}

// fetchGatewayConfig pulls the operator-rendered gateway config (openclaw.json)
// from the internal API. This is the pull-based replacement for waiting on the
// kubelet Secret-volume sync: the supervisor renders the file itself.
func (s *Supervisor) fetchGatewayConfig(ctx context.Context) ([]byte, error) {
	u := fmt.Sprintf("%s/internal/gateway/config", strings.TrimRight(s.cfg.APIURL, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// applyGatewayConfig writes the gateway config to the writable ConfigPath when
// its content changed, and reports whether a gateway restart is needed. Same
// content is a no-op, so the poll never restarts for an unchanged config.
func (s *Supervisor) applyGatewayConfig(data []byte) (bool, error) {
	if s.cfg.ConfigPath == "" {
		return false, nil
	}
	h := fmt.Sprintf("%x", sha256.Sum256(data))
	if h == s.lastCfgHash {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.ConfigPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(s.cfg.ConfigPath, data, 0o644); err != nil {
		return false, err
	}
	s.lastCfgHash = h
	return true, nil
}

// refreshGatewayConfig pulls the latest gateway config and applies it.
func (s *Supervisor) refreshGatewayConfig(ctx context.Context) (bool, error) {
	if s.cfg.ConfigPath == "" {
		return false, nil
	}
	data, err := s.fetchGatewayConfig(ctx)
	if err != nil {
		return false, err
	}
	return s.applyGatewayConfig(data)
}

// seedGatewayConfig ensures the writable gateway config exists before the
// gateway starts: pull it from the internal API (fast path), falling back to
// the read-only mounted Secret seed when the API is unreachable (cold start).
func (s *Supervisor) seedGatewayConfig(ctx context.Context) error {
	if s.cfg.ConfigPath == "" {
		return nil
	}
	if data, err := s.fetchGatewayConfig(ctx); err == nil {
		_, err := s.applyGatewayConfig(data)
		return err
	} else {
		log.Printf("supervisor: fetch gateway config: %v (seeding from mounted config)", err)
	}
	if s.cfg.SeedPath == "" {
		return nil
	}
	data, err := os.ReadFile(s.cfg.SeedPath)
	if err != nil {
		return err
	}
	_, err = s.applyGatewayConfig(data)
	return err
}

// poll fetches the resolved config and applies it (renders skills, records
// the revision). It reports whether the gateway must restart (revision
// changed). The gateway loads skills at startup, so a config change requires
// a graceful restart -- never a pod delete: sessions/PVC/IP survive.
func (s *Supervisor) poll(ctx context.Context) (bool, error) {
	cfg, err := s.fetchConfig(ctx)
	if err != nil {
		return false, err
	}
	if cfg.Empty() {
		// No instance config yet -- the gateway runs with its runtime
		// defaults; nothing to render.
		return false, nil
	}
	return s.applyConfig(ctx, cfg)
}

// applyConfig renders the skills when the revision changed and records it.
// Returns whether the gateway needs a restart.
func (s *Supervisor) applyConfig(ctx context.Context, cfg *resolver.ResolvedAgentConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == cfg.Revision {
		return false, nil // no change -- skills are current
	}
	log.Printf("supervisor: config revision %s -> %s", s.current, cfg.Revision)
	if err := s.renderSkills(ctx, cfg); err != nil {
		return false, fmt.Errorf("render skills: %w", err)
	}
	s.current = cfg.Revision
	return true, nil
}

// fetchConfig pulls the resolved agent config from the internal API.
func (s *Supervisor) fetchConfig(ctx context.Context) (*resolver.ResolvedAgentConfig, error) {
	u := fmt.Sprintf("%s/internal/agents/%s/config", strings.TrimRight(s.cfg.APIURL, "/"), url.PathEscape(s.cfg.User))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %d: %s", u, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cfg resolver.ResolvedAgentConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return &cfg, nil
}

// renderSkills writes the resolved domain skills into
// Workspace/skills/<name>/SKILL.md (clearing stale entries first). The
// gateway discovers skills under the workspace at startup.
func (s *Supervisor) renderSkills(ctx context.Context, cfg *resolver.ResolvedAgentConfig) error {
	skillsDir := filepath.Join(s.cfg.Workspace, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return err
	}
	// Clear the skills dir so removed skills disappear too.
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(skillsDir, e.Name())); err != nil {
			return err
		}
	}
	for _, skill := range cfg.Skills {
		rendered, err := resolver.RenderSkill(skill)
		if err != nil {
			return fmt.Errorf("render %s: %w", skill.Name, err)
		}
		dir := filepath.Join(skillsDir, skill.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", skill.Name, err)
		}
		log.Printf("supervisor: skill %s/%s rendered (%d bytes)", skill.Name, skill.Revision, len(rendered))
	}
	return nil
}

// startGateway launches the OpenClaw gateway as a child process.
func (s *Supervisor) startGateway(ctx context.Context) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if len(s.cfg.GatewayCmd) == 0 {
		return fmt.Errorf("no gateway command configured")
	}
	cmd := exec.CommandContext(ctx, s.cfg.GatewayCmd[0], s.cfg.GatewayCmd[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	log.Printf("supervisor: gateway started (pid %d)", cmd.Process.Pid)
	return nil
}

// stopGateway terminates the gateway process if running.
func (s *Supervisor) stopGateway() {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
}

// restartGateway gracefully restarts the gateway process (SIGTERM -> wait ->
// start). The pod and its PVC/IP survive; only the process is recycled.
func (s *Supervisor) restartGateway(ctx context.Context) error {
	s.stopGateway()
	return s.startGateway(ctx)
}
