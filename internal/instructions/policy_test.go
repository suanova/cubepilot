package instructions

import (
	"strings"
	"testing"
)

func TestValidateCountsCharactersInsteadOfUTF8Bytes(t *testing.T) {
	if err := Validate(strings.Repeat("界", MaxChars)); err != nil {
		t.Fatalf("MaxChars CJK prompt rejected: %v", err)
	}
	if err := Validate(strings.Repeat("界", MaxChars+1)); err == nil {
		t.Fatal("prompt above MaxChars was accepted")
	}
}

func TestValidateRejectsManagedMarkers(t *testing.T) {
	if err := Validate("before\n" + ManagedStart + "\nafter"); err == nil {
		t.Fatal("managed marker was accepted")
	}
}
