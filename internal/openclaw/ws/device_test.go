package ws

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestSignaturePayloadGoldenV3(t *testing.T) {
	p := signInput{
		DeviceID: "dev", ClientID: "gateway-client", ClientMode: "backend",
		Role: "operator", Scopes: []string{"operator.admin", "operator.read"},
		SignedAtMs: 1234567890, Token: "tok", Nonce: "nonce-1",
		Platform: "  Linux ", DeviceFamily: "cubepilot",
	}
	got := signaturePayload(p)
	want := "v3|dev|gateway-client|backend|operator|operator.admin,operator.read|1234567890|tok|nonce-1|linux|cubepilot"
	if got != want {
		t.Errorf("signaturePayload:\n got %q\nwant %q", got, want)
	}
}

func TestDeviceIdentityAndSign(t *testing.T) {
	dev, err := GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	// device.id == hex(sha256(raw pub)); publicKey == base64url(raw pub).
	sum := sha256Hex(dev.pubRaw)
	if dev.ID != sum {
		t.Errorf("device.ID = %q, want %q", dev.ID, sum)
	}
	if want := base64.RawURLEncoding.EncodeToString(dev.pubRaw); dev.PublicKey != want {
		t.Errorf("PublicKey = %q, want %q", dev.PublicKey, want)
	}

	p := signInput{
		DeviceID: dev.ID, ClientID: "gateway-client", ClientMode: "backend",
		Role: "operator", Scopes: []string{"operator.admin"},
		SignedAtMs: 1700000000000, Token: "secret", Nonce: "n",
		Platform: "linux", DeviceFamily: "",
	}
	sigB64, err := dev.SignProof(p)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("signature not valid base64url: %v", err)
	}
	pub := ed25519.PublicKey(dev.pubRaw)
	if !ed25519.Verify(pub, []byte(signaturePayload(p)), sig) {
		t.Fatal("signature did not verify over the canonical payload")
	}
	// The signature must not be the same for a different nonce.
	p.Nonce = "other"
	sigB64b, _ := dev.SignProof(p)
	if sigB64 == sigB64b {
		t.Error("signature must change with the nonce")
	}
}
