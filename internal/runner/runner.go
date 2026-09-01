// Package runner implements the scheduler's Runner interface by talking to
// the creator's agent instance directly (OpenClaw gateway), so the operator
// process does not depend on the API process.
package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/suanova/cubepilot/internal/openclaw"
)

// manager is the narrow slice of the Instance Manager that Runner depends on:
// warm the instance, resolve its gateway URL and the per-turn model override.
// Depending on the interface (not *instances.Manager) keeps Runner
// unit-testable and lets the scheduler run against any resolution backend.
type manager interface {
	Ensure(ctx context.Context, user string) error
	BaseURL(user string) string
	SelectedModelFor(ctx context.Context, user string) (string, error)
}

// Runner executes one task turn through the creator's agent instance and
// returns the collected text.
type Runner struct {
	mgr   manager
	token string
}

// New returns a Runner for the given instance manager and gateway token
// (shared across agent instances). *instances.Manager satisfies manager.
func New(mgr manager, token string) *Runner {
	return &Runner{mgr: mgr, token: token}
}

// RunTask runs one task turn: prompt -> agent instance -> collected deltas.
// The instance is warmed (CR phase Warm + gateway reachable) before the
// stream starts.
func (r *Runner) RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error) {
	if err := r.mgr.Ensure(ctx, creator); err != nil {
		return "", fmt.Errorf("instance warming failed: %w", err)
	}
	client := openclaw.New(r.mgr.BaseURL(creator), r.token) // openclaw.AgentRuntime
	// Apply the creator's selectedModel the same way the chat path does
	// (design §3.2/§3.3: fail-closed on an unavailable selection).
	if model, err := r.mgr.SelectedModelFor(ctx, creator); err != nil {
		return "", fmt.Errorf("model resolution: %w", err)
	} else if model != "" {
		client.SetModel(model)
	}
	var buf strings.Builder
	var doneErr string
	err := client.StreamChat(ctx, openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   []openclaw.ChatMessage{{Role: "user", Content: prompt}},
	}, func(ev openclaw.Event) error {
		switch ev.Type {
		case openclaw.EventMessageDelta:
			buf.WriteString(ev.Delta)
		case openclaw.EventMessageDone:
			doneErr = ev.Error
		}
		return nil
	})
	if err != nil {
		return buf.String(), err
	}
	if doneErr != "" {
		return buf.String(), fmt.Errorf("%s", doneErr)
	}
	return buf.String(), nil
}
