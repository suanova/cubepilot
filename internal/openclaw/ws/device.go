// Package ws implements a minimal gateway-protocol WebSocket client for the
// OpenClaw gateway (v2026.8.2), authenticated as a paired operator device. It
// exists to drive the exec-approval HITL surface (issue #20): subscribe to
// exec approval broadcasts, resolve them, author the per-agent exec-approvals
// policy, and set a session's permissionMode. The platform keeps chat on HTTP;
// this WS client is the approvals/control channel.
//
// The wire shapes here are ported from OpenClaw TS at tag v2026.8.2
// (packages/gateway-protocol/src/schema/frames.ts, device-auth.ts,
// connect-device-proof.ts, server-methods/*). Protocol version on the wire is 4.
package ws

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// protocolVersion is the gateway-protocol version the client negotiates.
const protocolVersion = 4

// scopes for the device connection. operator.admin is included so every method
// gate passes and approval records stay visible (the gateway trims scopes down
// to what the paired device row approves; use the hello auth.scopes as truth).
var defaultScopes = []string{"operator.admin", "operator.read", "operator.write", "operator.approvals"}

// caps advertised so the gateway fans exec-approval broadcasts to this client.
var defaultCaps = []string{"approvals", "exec-approvals"}

// Device is an Ed25519 operator identity paired with the gateway.
type Device struct {
	priv      ed25519.PrivateKey
	pubRaw    []byte // raw 32-byte Ed25519 public key
	ID        string // hex(sha256(pubRaw)), the wire device.id
	PublicKey string // base64url (unpadded) of pubRaw, the wire device.publicKey
}

// GenerateDevice creates a fresh device identity.
func GenerateDevice() (*Device, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("ed25519 key: %w", err)
	}
	return NewDevice(pub, priv)
}

// NewDevice wraps an existing Ed25519 keypair.
func NewDevice(pub ed25519.PublicKey, priv ed25519.PrivateKey) (*Device, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key size %d != %d", len(pub), ed25519.PublicKeySize)
	}
	raw := append([]byte(nil), pub...)
	sum := sha256.Sum256(raw)
	return &Device{
		priv:      priv,
		pubRaw:    raw,
		ID:        hex.EncodeToString(sum[:]),
		PublicKey: base64.RawURLEncoding.EncodeToString(raw),
	}, nil
}

// signInput is everything the canonical device signature covers (device-auth.ts
// v3 layout).
type signInput struct {
	DeviceID     string
	ClientID     string
	ClientMode   string
	Role         string
	Scopes       []string
	SignedAtMs   int64
	Token        string // auth.token ?? deviceToken ?? bootstrapToken
	Nonce        string // challenge nonce
	Platform     string
	DeviceFamily string
}

// signaturePayload builds the exact | -joined canonical string that is signed.
func signaturePayload(p signInput) string {
	platform := normalizeAuthMetadata(p.Platform)
	family := normalizeAuthMetadata(p.DeviceFamily)
	return strings.Join([]string{
		"v3",
		p.DeviceID,
		p.ClientID,
		p.ClientMode,
		p.Role,
		strings.Join(p.Scopes, ","),
		strconv.FormatInt(p.SignedAtMs, 10),
		p.Token,
		p.Nonce,
		platform,
		family,
	}, "|")
}

// normalizeAuthMetadata mirrors normalizeDeviceMetadataForAuth: trim, lowercase.
func normalizeAuthMetadata(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// SignProof returns the base64url(Ed25519(payload)) signature over the v3
// canonical payload.
func (d *Device) SignProof(p signInput) (string, error) {
	if p.DeviceID == "" {
		p.DeviceID = d.ID
	}
	payload := signaturePayload(p)
	sig := ed25519.Sign(d.priv, []byte(payload))
	return base64.RawURLEncoding.EncodeToString(sig), nil
}
