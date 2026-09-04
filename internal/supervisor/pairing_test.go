package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

func TestDeviceIDFromPublicKeyMatchesOpenClaw(t *testing.T) {
	dev, err := ws.GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	got, err := deviceIDFromPublicKey(dev.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if got != dev.ID {
		t.Errorf("deviceIDFromPublicKey = %q, want %q (OpenClaw deriveDeviceIdFromPublicKey)", got, dev.ID)
	}
	if _, err := deviceIDFromPublicKey("not-base64!!"); err == nil {
		t.Error("expected an error for an invalid public key")
	}
}

func TestGatewayPortFromCmd(t *testing.T) {
	if got := gatewayPortFromCmd([]string{"node", "dist/index.js", "gateway", "--bind", "lan", "--port", "18789"}); got != 18789 {
		t.Errorf("port = %d, want 18789", got)
	}
	if got := gatewayPortFromCmd([]string{"gateway"}); got != 0 {
		t.Errorf("port = %d, want 0 when absent", got)
	}
}

func TestGatewayTokenFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(path, []byte(`{"gateway":{"auth":{"token":"tok-123"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := gatewayTokenFromConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok-123" {
		t.Errorf("token = %q", tok)
	}
	if _, err := gatewayTokenFromConfig(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("expected an error for a missing config file")
	}
}
