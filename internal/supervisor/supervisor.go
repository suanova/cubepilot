// Package supervisor implements the agent-pod-side runtime supervisor: it
// pulls the resolved agent config from the platform internal API, renders
// domain skills into the OpenClaw workspace as skills, and manages the
// OpenClaw gateway process (graceful restart on config change -- the pod is
// never deleted, so sessions/PVC/IP survive). It is the "agent supervisor"
// of the final architecture: CRDs declare -> operator resolves -> supervisor
// renders -> OpenClaw executes.
//
// The gateway config (LLM providers / model allowlist, mounted from the
// openclaw-config Secret as openclaw.json) is also watched: the gateway loads
// it at startup, so when the file changes the gateway is gracefully restarted.
// This is how providers are added/edited post-install (by patching the Secret)
// without touching Pods or scripts/setup.sh.
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
	// ConfigPath is the gateway config file (openclaw.json, mounted from the
	// openclaw-config Secret). The gateway reads it at startup; when it changes
	// the supervisor restarts the gateway so the new config applies. Empty
	// disables the file watch.
	ConfigPath string
}

// LoadFromEnv builds a Config from the environment with sane defaults.
func LoadFromEnv() Config {
	return Config{
		APIURL:       getenv("CUBEPILOT_API_URL", "http://cubepilot-api.cubepilot.svc:8080"),
		User:         os.Getenv("CUBEPILOT_AGENT_USER"),
		Workspace:    getenv("CUBEPILOT_WORKSPACE", "/home/node/.openclaw/workspace"),
		GatewayCmd:   []string{"node", "dist/index.js", "gateway", "--bind", "lan", "--port", "18789"},
		PollInterval: 10 * time.Second,
		ConfigPath:   getenv("OPENCLAW_CONFIG_PATH", "/home/node/.openclaw/config/openclaw.json"),
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
	if err := s.startGateway(ctx); err != nil {
		return fmt.Errorf("start gateway: %w", err)
	}
	// Baseline the gateway config file so edits (e.g. a provider added to the
	// openclaw-config Secret post-install) are detected relative to what the
	// gateway just loaded, not treated as a change on the first tick.
	s.snapshotConfigHash()

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
			if s.configHashChanged() {
				log.Printf("supervisor: gateway config file changed; restarting")
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

// hashFile returns the sha256 hex of the gateway config file, or "" when the
// file is missing (e.g. the Secret is not mounted yet).
func (s *Supervisor) hashFile() string {
	data, err := os.ReadFile(s.cfg.ConfigPath)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

// snapshotConfigHash records the current gateway config file as the baseline.
func (s *Supervisor) snapshotConfigHash() {
	s.lastCfgHash = s.hashFile()
}

// configHashChanged reports whether the gateway config file differs from the
// last snapshot (and records the new hash). A missing file reports no change
// but does not reset the baseline, so a transient mount gap never triggers a
// restart. No-op when no ConfigPath is configured.
func (s *Supervisor) configHashChanged() bool {
	if s.cfg.ConfigPath == "" {
		return false
	}
	h := s.hashFile()
	if h == "" || h == s.lastCfgHash {
		return false
	}
	s.lastCfgHash = h
	return true
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
