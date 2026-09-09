// Package instructions defines the safety boundary for instructions rendered
// into an agent workspace's managed AGENTS.md section.
package instructions

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// ManagedStart and ManagedEnd delimit the section owned by CubePilot.
	ManagedStart = "<!-- cubepilot:system-prompt:start -->"
	ManagedEnd   = "<!-- cubepilot:system-prompt:end -->"

	// MaxChars matches OpenClaw's default per-file bootstrap character budget.
	// Count Unicode code points so non-ASCII instructions receive the same
	// usable budget as English text.
	MaxChars = 20_000
)

// Validate rejects content that cannot be safely rendered into the managed
// AGENTS.md section.
func Validate(value string) error {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > MaxChars {
		return fmt.Errorf("instructions exceed the %d-character limit", MaxChars)
	}
	if strings.Contains(value, ManagedStart) || strings.Contains(value, ManagedEnd) {
		return fmt.Errorf("instructions contain a reserved managed-section marker")
	}
	return nil
}
