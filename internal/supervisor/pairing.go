// Device-pairing bootstrap (issue #20 HITL). The platform talks to the gateway
// over the network as a paired operator device; OpenClaw only grants a device
// its requested role/scopes once an operator has paired it. This supervisor
// runs inside the agent Pod (same network namespace as the gateway), where a
// device-less `gateway-client`/`backend` connection with the shared token is
// admitted with full operator scopes (loopback carve-out) -- so it acts as the
// bootstrap admin that approves the platform device's pending pairing.
package supervisor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

// deviceIDFromPublicKey derives the gateway device id from an unpadded
// base64url Ed25519 public key: hex(sha256(raw 32 bytes)) -- same derivation
// OpenClaw uses (src/infra/device-identity.ts).
func deviceIDFromPublicKey(publicKeyB64 string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return "", fmt.Errorf("decode device public key: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// gatewayTokenFromConfig reads the shared gateway token from the rendered
// openclaw.json the supervisor maintains (gateway.auth.token).
func gatewayTokenFromConfig(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var cfg struct {
		Gateway struct {
			Auth struct {
				Token string `json:"token"`
			} `json:"auth"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", err
	}
	if cfg.Gateway.Auth.Token == "" {
		return "", fmt.Errorf("no gateway.auth.token in %s", path)
	}
	return cfg.Gateway.Auth.Token, nil
}

// gatewayPortFromCmd extracts the --port value from the gateway argv.
func gatewayPortFromCmd(argv []string) int {
	for i, a := range argv {
		if a == "--port" && i+1 < len(argv) {
			var p int
			if _, err := fmt.Sscanf(argv[i+1], "%d", &p); err == nil && p > 0 {
				return p
			}
		}
	}
	return 0
}

// ensureDevicePaired approves the platform's device for this gateway once.
// It reads the desired device public key from the resolved config (served by
// the platform); when HITL is off the field is empty and this is a no-op.
// Called from the supervisor's poll loop (Run goroutine only).
func (s *Supervisor) ensureDevicePaired(ctx context.Context) {
	cfg := s.lastCfg
	if cfg == nil || cfg.DevicePublicKey == "" {
		return
	}
	if s.devicePaired {
		return
	}
	deviceID, err := deviceIDFromPublicKey(cfg.DevicePublicKey)
	if err != nil {
		log.Printf("pairing: bad device public key from platform: %v", err)
		return
	}
	token, err := gatewayTokenFromConfig(s.cfg.ConfigPath)
	if err != nil {
		return // openclaw.json not ready yet; retry next poll
	}
	port := gatewayPortFromCmd(s.cfg.GatewayCmd)
	if port == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cli := ws.NewClient(fmt.Sprintf("ws://127.0.0.1:%d/gateway", port), token, nil)
	if err := cli.Connect(ctx); err != nil {
		log.Printf("pairing: loopback admin connect: %v", err)
		return
	}
	defer cli.Close()

	lst, err := cli.DevicePairList(ctx)
	if err != nil {
		log.Printf("pairing: device.pair.list: %v", err)
		return
	}
	for _, pd := range lst.Paired {
		if pd.DeviceID == deviceID {
			s.devicePaired = true // already paired -- done
			return
		}
	}
	for _, p := range lst.Pending {
		if p.DeviceID == deviceID {
			if err := cli.DevicePairApprove(ctx, p.RequestID); err != nil {
				log.Printf("pairing: approve %s: %v", deviceID, err)
				return
			}
			s.devicePaired = true
			log.Printf("pairing: approved platform device %s", deviceID)
			return
		}
	}
	// No pending request for our device yet -- the platform has not tried to
	// connect (or its request is not visible); retry on the next poll.
}
