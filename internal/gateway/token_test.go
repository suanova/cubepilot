package gateway

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureGatewayToken(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	tok, err := EnsureGatewayToken(ctx, cl, "cubepilot")
	if err != nil {
		t.Fatalf("EnsureGatewayToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64", len(tok))
	}

	// Second call reuses the persisted token (never regenerates).
	tok2, err := EnsureGatewayToken(ctx, cl, "cubepilot")
	if err != nil {
		t.Fatalf("EnsureGatewayToken #2: %v", err)
	}
	if tok2 != tok {
		t.Errorf("token changed: %q != %q", tok2, tok)
	}

	// A pre-existing token is preserved.
	sec := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-config", Namespace: "cubepilot"}}
	sec.Data = map[string][]byte{"gatewayToken": []byte("pre-existing")}
	if err := cl.Update(ctx, &sec); err != nil {
		t.Fatalf("update: %v", err)
	}
	tok3, err := EnsureGatewayToken(ctx, cl, "cubepilot")
	if err != nil {
		t.Fatalf("EnsureGatewayToken #3: %v", err)
	}
	if tok3 != "pre-existing" {
		t.Errorf("did not reuse existing token: %q", tok3)
	}
}
