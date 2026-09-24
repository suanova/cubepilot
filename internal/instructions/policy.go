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

	// PersonaReserveChars is the share of the managed-section budget that the
	// platform's own text occupies: the persona, plus the heading and separators
	// that introduce the instructions section. Instructions are accepted against
	// MaxChars-PersonaReserveChars so that a set the API accepts always fits the
	// section it is rendered into -- a block that does not fit is skipped by the
	// supervisor, which would leave the operator with a saved prompt that is never
	// delivered and no error to see.
	//
	// The reserve must cover the real persona; a test in internal/supervisor pins
	// that, because that package is where the persona text lives.
	PersonaReserveChars = 3_200
)

// Validate rejects a user- or operator-supplied instruction set that cannot be
// safely rendered into the managed AGENTS.md section.
//
// The limit is MaxChars-PersonaReserveChars rather than the whole file budget:
// the section it is rendered into also carries the platform's own text, and the
// supervisor skips a composed block that exceeds the file budget. Enforcing the
// smaller limit here is what makes an accepted value always deliverable.
func Validate(value string) error {
	return validateAgainst(value, MaxChars-PersonaReserveChars)
}

// ValidateRendered rejects a composed managed-section body -- the platform text
// and the instructions together -- that cannot be safely rendered.
//
// The renderer uses this instead of Validate because it composes the persona
// with the instructions, so the budget that applies to what it builds is the
// whole section's. The value passed to Validate is only the instructions.
func ValidateRendered(body string) error {
	return validateAgainst(body, MaxChars)
}

// validateAgainst is the shared shape of both checks: the character limit and
// the reserved-marker check, measured against the budget the caller applies.
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
