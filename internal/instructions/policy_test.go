package instructions

import (
	"fmt"
	"strings"
	"testing"
)

// instructionBudget is the largest instruction set Validate accepts. It is
// deliberately smaller than MaxChars: the section a value is rendered into also
// carries the platform's persona.
const instructionBudget = MaxChars - PersonaReserveChars

func TestValidateCountsCharactersInsteadOfUTF8Bytes(t *testing.T) {
	if err := Validate(strings.Repeat("界", instructionBudget)); err != nil {
		t.Fatalf("%d-rune CJK prompt rejected: %v", instructionBudget, err)
	}
	if err := Validate(strings.Repeat("界", instructionBudget+1)); err == nil {
		t.Fatalf("prompt above the %d-rune budget was accepted", instructionBudget)
	}
}

// TestValidateEnforcesTheRenderedBudget pins the limit Validate enforces and the
// number its error names: a value that passes here must fit the section once the
// persona is composed into it, so the limit is the file budget minus the reserve
// -- and a wrong number in the message would send an operator trimming to a size
// the API still rejects.
func TestValidateEnforcesTheRenderedBudget(t *testing.T) {
	if err := Validate(strings.Repeat("x", instructionBudget)); err != nil {
		t.Fatalf("value at exactly the budget rejected: %v", err)
	}
	err := Validate(strings.Repeat("x", instructionBudget+1))
	if err == nil {
		t.Fatalf("value above the budget (%d) was accepted", instructionBudget)
	}
	if want := fmt.Sprintf("%d-character limit", instructionBudget); !strings.Contains(err.Error(), want) {
		t.Errorf("error should name the enforced limit %q, got %q", want, err.Error())
	}
}

// TestValidateRejectsManagedMarkers keeps the marker check on the path the API
// uses: an embedded marker would be read as the block's end and grow the file on
// every poll.
func TestValidateRejectsManagedMarkers(t *testing.T) {
	if err := Validate("before\n" + ManagedStart + "\nafter"); err == nil {
		t.Fatal("managed start marker was accepted")
	}
	if err := Validate("before\n" + ManagedEnd + "\nafter"); err == nil {
		t.Fatal("managed end marker was accepted")
	}
}

// TestValidateRenderedCountsAgainstTheWholeFile is the other side of the same
// budget: the renderer passes the composed body (persona + instructions), so its
// limit is the whole section.
func TestValidateRenderedCountsAgainstTheWholeFile(t *testing.T) {
	if err := ValidateRendered(strings.Repeat("界", MaxChars)); err != nil {
		t.Fatalf("body at MaxChars rejected: %v", err)
	}
	err := ValidateRendered(strings.Repeat("界", MaxChars+1))
	if err == nil {
		t.Fatal("body above MaxChars was accepted")
	}
	if want := fmt.Sprintf("%d-character limit", MaxChars); !strings.Contains(err.Error(), want) {
		t.Errorf("error should name the whole-file limit %q, got %q", want, err.Error())
	}
}

func TestValidateRenderedRejectsManagedMarkers(t *testing.T) {
	if err := ValidateRendered("body\n" + ManagedStart + "\n"); err == nil {
		t.Fatal("managed start marker was accepted")
	}
	if err := ValidateRendered("body\n" + ManagedEnd + "\n"); err == nil {
		t.Fatal("managed end marker was accepted")
	}
}
