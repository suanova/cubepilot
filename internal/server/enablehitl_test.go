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

// hitlTestServer builds a Server whose HITL channel can be enabled against a
// fake client seeded with objs.
func hitlTestServer(t *testing.T, objs ...client.Object) *Server {
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

// TestEnableHITLAutoCreatesMasterSecret verifies EnableHITL generates and
// persists the master key Secret on first enable -- no operator-supplied key is
// needed (issue #127).
func TestEnableHITLAutoCreatesMasterSecret(t *testing.T) {
	s := hitlTestServer(t)
	if err := s.EnableHITL(); err != nil {
		t.Fatalf("EnableHITL: %v", err)
	}
	if s.hitl == nil {
		t.Fatal("hitl manager not configured after EnableHITL")
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), client.ObjectKey{Name: hitlMasterSecretName, Namespace: "test"}, &sec); err != nil {
		t.Fatalf("read master Secret: %v", err)
	}
	if len(sec.Data["key"]) == 0 {
		t.Fatal("master Secret has no key")
	}
}

// TestEnableHITLReusesPersistedKey verifies a restarted API re-uses the key
// already in the Secret so the derived per-user device identities stay stable
// (issue #128).
func TestEnableHITLReusesPersistedKey(t *testing.T) {
	const key = "stable-key-32-bytes-long!!!!!"
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hitlMasterSecretName, Namespace: "test"},
		Data:       map[string][]byte{"key": []byte(base64.StdEncoding.EncodeToString([]byte(key)))},
	}
	s := hitlTestServer(t, existing)
	if err := s.EnableHITL(); err != nil {
		t.Fatalf("EnableHITL: %v", err)
	}
	if s.hitl == nil {
		t.Fatal("hitl manager not configured")
	}
	if string(s.hitl.masterKey) != key {
		t.Fatalf("master key not re-used: %q", s.hitl.masterKey)
	}
}

// TestEnableHITLFailsOnInvalidMasterKey verifies a corrupt persisted key makes
// EnableHITL return an error instead of silently running without HITL (issue
// #127/#128) -- the caller treats the failure as fatal.
func TestEnableHITLFailsOnInvalidMasterKey(t *testing.T) {
	corrupt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hitlMasterSecretName, Namespace: "test"},
		Data:       map[string][]byte{"key": []byte("!!!not-base64!!!")},
	}
	s := hitlTestServer(t, corrupt)
	if err := s.EnableHITL(); err == nil {
		t.Fatal("EnableHITL should error on a corrupt master key")
	}
	if s.hitl != nil {
		t.Fatal("hitl manager configured despite the corrupt master key")
	}
}

// TestEnableHITLFailsWithoutGatewayToken verifies an API that cannot wire the
// approval channel reports the failure instead of half-configuring (issue
// #127).
func TestEnableHITLFailsWithoutGatewayToken(t *testing.T) {
	s := hitlTestServer(t)
	s.cfg.GatewayToken = ""
	if err := s.EnableHITL(); err == nil {
		t.Fatal("EnableHITL should error when the gateway token is missing")
	}
	if s.hitl != nil {
		t.Fatal("hitl manager configured without a gateway token")
	}
}
