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
// the internal API (GET /internal/gateway/config) into openclaw's default
// config path; when the content changes the gateway is gracefully restarted.
// This is how providers are added/edited post-install without touching Pods or
// scripts/setup.sh.
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
	// ConfigPath is the gateway config file (openclaw.json) the supervisor
	// renders from the internal API; the gateway reads it from openclaw's
	// default path. When the pulled content changes the supervisor restarts the
	// gateway so the new config applies. Empty disables the pull.
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
		ConfigPath:   getenv("OPENCLAW_CONFIG_PATH", "/home/node/.openclaw/openclaw.json"),
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

	cmdMu  sync.Mutex
	cmd    *exec.Cmd
	waitCh chan error // receives the gateway child's Wait result when it exits

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

// Run is the supervisor main loop: wait for the initial config, start the
// gateway, then poll the resolved config and write rendered skills + gateway
// config. The gateway owns its own reload: its config reloader watches
// openclaw.json (chokidar) and re-scans workspace skills, so the supervisor
// never restarts the gateway for a config change. It does respawn the gateway
// if the child process crashes (the operator only re-creates Failed pods, so
// a dead gateway inside a live supervisor would otherwise wedge the pod
// NotReady forever), and stops the gateway on shutdown.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.cfg.User == "" {
		return fmt.Errorf("CUBEPILOT_AGENT_USER is required")
	}
	// The gateway refuses to boot without a valid openclaw.json
	// (gateway.mode=local), so wait for the resolved config and the rendered
	// gateway config to apply before starting it. Booting earlier would leave
	// the gateway permanently unready: the gateway reloads config itself and
	// the supervisor never restarts it, so there is no second chance.
	if err := s.waitForInitialConfig(ctx); err != nil {
		return err
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
			// Crash recovery: the gateway child exited unexpectedly -> respawn.
			// Config changes never trigger this; the gateway reloads itself.
			select {
			case werr := <-s.waitCh:
				log.Printf("supervisor: gateway exited (%v); respawning", werr)
				if err := s.startGateway(ctx); err != nil {
					log.Printf("supervisor: respawn: %v", err)
				}
			default:
			}
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
			// The gateway reloads itself: its config reloader watches
			// openclaw.json, so writing the updated files is sufficient.
			if changed || gcChanged {
				log.Printf("supervisor: config updated (resolved=%v gateway=%v); OpenClaw reloads it", changed, gcChanged)
			}
		}
	}
}

// waitForInitialConfig retries fetching the resolved agent config and the
// rendered gateway config until both apply. The OpenClaw gateway refuses to
// boot without gateway.mode=local, so the supervisor waits rather than start a
// gateway that can never become ready (nothing would restart it later).
// Backoff doubles up to a cap; the context bounds the wait.
func (s *Supervisor) waitForInitialConfig(ctx context.Context) error {
	backoff := 2 * time.Second
	for {
		_, pollErr := s.poll(ctx)
		if pollErr != nil {
			log.Printf("supervisor: initial config poll: %v (retrying)", pollErr)
		}
		_, gwErr := s.refreshGatewayConfig(ctx)
		if gwErr != nil {
			log.Printf("supervisor: initial gateway config fetch: %v (retrying)", gwErr)
		}
		if pollErr == nil && gwErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
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

// poll fetches the resolved config and applies it (renders skills, records
// the revision). It reports whether the resolved config changed so the caller
// can log it; the gateway reloads skills itself, so no restart is needed.
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
// Returns whether the resolved config changed (for logging only).
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
	// Reap the child and signal its exit so Run can respawn it on crash.
	s.waitCh = make(chan error, 1)
	go func() {
		s.waitCh <- cmd.Wait()
	}()
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
	if s.waitCh != nil {
		select {
		case <-s.waitCh: // the Wait goroutine reaps the child
		case <-time.After(30 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.waitCh
		}
	}
	s.cmd = nil
}

