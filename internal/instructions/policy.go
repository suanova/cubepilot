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

	// PersonaReserveChars is the budget the platform's own text occupies in the managed
	// section -- the persona plus the heading and separators -- so that a set the API
	// accepts always fits the block it is rendered into.
	PersonaReserveChars = 3_200
)

// Validate rejects a user- or operator-supplied instruction set that cannot be safely
// rendered into the managed AGENTS.md section. It measures against MaxChars minus the
// persona's reserve, because the block it renders into carries the platform text too.
func Validate(value string) error {
	return validateAgainst(value, MaxChars-PersonaReserveChars)
}

// ValidateRendered rejects a composed managed-section body -- the platform text and the
// instructions together -- that cannot be safely rendered; it is the check for what the
// renderer actually writes.
func ValidateRendered(body string) error {
	return validateAgainst(body, MaxChars)
}

// validateAgainst applies the character limit and the reserved-marker check.
func validateAgainst(value string, limit int) error {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > limit {
		return fmt.Errorf("instructions exceed the %d-character limit", limit)
	}
	if strings.Contains(value, ManagedStart) || strings.Contains(value, ManagedEnd) {
		return fmt.Errorf("instructions contain a reserved managed-section marker")
	}
	return nil
}
