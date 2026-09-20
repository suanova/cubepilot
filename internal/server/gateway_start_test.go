package server

import (
	"context"
	"encoding/base64"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
)

// gatewayStartTestServer builds a Server whose gateway channel can be started
// against a fake client seeded with objs.
func gatewayStartTestServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	cfg := config.Config{Namespace: "test", GatewayToken: "tok", DefaultUser: "zhang.wei"}
	return New(cfg, instances.New(cl, cfg), nil, nil, cl)
}

// TestStartGatewayChannelAutoCreatesRootSecret verifies StartGatewayChannel
// generates and persists the device root key Secret on first start -- no
// operator-supplied key is needed (issue #127).
func TestStartGatewayChannelAutoCreatesRootSecret(t *testing.T) {
	s := gatewayStartTestServer(t)
	if err := s.StartGatewayChannel(); err != nil {
		t.Fatalf("StartGatewayChannel: %v", err)
	}
	if s.gatewayConns == nil {
		t.Fatal("gateway manager not configured after StartGatewayChannel")
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), client.ObjectKey{Name: deviceRootSecretName, Namespace: "test"}, &sec); err != nil {
		t.Fatalf("read device root Secret: %v", err)
	}
	if len(sec.Data["key"]) == 0 {
		t.Fatal("device root Secret has no key")
	}
}

// TestStartGatewayChannelReusesPersistedKey verifies a restarted API re-uses the key
// already in the Secret so the derived per-user device identities stay stable
// (issue #128).
func TestStartGatewayChannelReusesPersistedKey(t *testing.T) {
	const key = "stable-key-32-bytes-long!!!!!"
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: deviceRootSecretName, Namespace: "test"},
		Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString([]byte(key)))},
	}
	s := gatewayStartTestServer(t, existing)
	if err := s.StartGatewayChannel(); err != nil {
		t.Fatalf("StartGatewayChannel: %v", err)
	}
	if s.gatewayConns == nil {
		t.Fatal("gateway manager not configured")
	}
	if string(s.gatewayConns.rootKey) != key {
		t.Fatalf("device root key not re-used: %q", s.gatewayConns.rootKey)
	}
}

// TestStartGatewayChannelFailsOnInvalidRootKey verifies a corrupt persisted key
// makes StartGatewayChannel return an error instead of silently running without
// a channel (issue #127/#128) -- the caller treats the failure as fatal.
func TestStartGatewayChannelFailsOnInvalidRootKey(t *testing.T) {
	corrupt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: deviceRootSecretName, Namespace: "test"},
		Data:       map[string][]byte{"key": []byte("!!!not-base64!!!")},
	}
	s := gatewayStartTestServer(t, corrupt)
	if err := s.StartGatewayChannel(); err == nil {
		t.Fatal("StartGatewayChannel should error on a corrupt device root key")
	}
	if s.gatewayConns != nil {
		t.Fatal("gateway manager configured despite the corrupt device root key")
	}
}

// TestStartGatewayChannelFailsWithoutGatewayToken verifies an API that cannot
// wire the channel reports the failure instead of half-configuring (issue
// #127).
func TestStartGatewayChannelFailsWithoutGatewayToken(t *testing.T) {
	s := gatewayStartTestServer(t)
	s.cfg.GatewayToken = ""
	if err := s.StartGatewayChannel(); err == nil {
		t.Fatal("StartGatewayChannel should error when the gateway token is missing")
	}
	if s.gatewayConns != nil {
		t.Fatal("gateway manager configured without a gateway token")
	}
}
