// Package instructions defines the safety boundary for instructions rendered
// into an agent workspace's managed AGENTS.md section.
package instructions

import (
	"fmt"
	"strings"
)

const (
	// ManagedStart and ManagedEnd delimit the section owned by CubePilot.
	ManagedStart = "<!-- cubepilot:system-prompt:start -->"
	ManagedEnd   = "<!-- cubepilot:system-prompt:end -->"

	// MaxBytes keeps the managed prompt below a reasonable per-turn bootstrap
	// cost and below OpenClaw's own bootstrap truncation boundary.
	MaxBytes = 32 << 10
)

// Validate rejects content that cannot be safely rendered into the managed
// AGENTS.md section.
func Validate(value string) error {
	value = strings.TrimSpace(value)
	if len(value) > MaxBytes {
		return fmt.Errorf("instructions exceed the %d-byte limit", MaxBytes)
	}
	if strings.Contains(value, ManagedStart) || strings.Contains(value, ManagedEnd) {
		return fmt.Errorf("instructions contain a reserved managed-section marker")
	}
	return nil
}
