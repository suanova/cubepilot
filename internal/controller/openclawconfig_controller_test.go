package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

func TestOpenClawConfigReconcile(t *testing.T) {
	scheme := testScheme(t)
	builtin := BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-llm", Namespace: "cubepilot"},
		Data:       map[string][]byte{"apiKey": []byte("sk-real")},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(builtin, cred).Build()
	r := &OpenClawConfigReconciler{Client: cl, Scheme: scheme, Cfg: config.Config{Namespace: "cubepilot"}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sec corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: k8s.ConfigSecretName}, &sec); err != nil {
		t.Fatalf("openclaw-config not created: %v", err)
	}
	jsonData := string(sec.Data["openclaw.json"])
	if !strings.Contains(jsonData, `"deepseek-v4-flash/deepseek-v4-flash"`) {
		t.Errorf("openclaw.json missing primary ref: %s", jsonData)
	}
	if !strings.Contains(jsonData, `"apiKey": "$CUBEPILOT_LLM_DEEPSEEK_V4_FLASH"`) {
		t.Errorf("openclaw.json apiKey should be an env ref ($CUBEPILOT_LLM_...): %s", jsonData)
	}
	if strings.Contains(jsonData, "sk-real") {
		t.Errorf("openclaw.json must not contain the literal credential: %s", jsonData)
	}
	if tok := string(sec.Data["gatewayToken"]); len(tok) != 64 {
		t.Errorf("gatewayToken length = %d, want 64", len(tok))
	}
}

func TestOpenClawConfigReconcileSkipsMissingCredential(t *testing.T) {
	scheme := testScheme(t)
	builtin := BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash") // references cubepilot-llm, which is absent
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(builtin).Build()
	r := &OpenClawConfigReconciler{Client: cl, Scheme: scheme, Cfg: config.Config{Namespace: "cubepilot"}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Missing credential: the Secret is still created (no providers), and no
	// error is returned -- the controller keeps requeueing.
	var sec corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: k8s.ConfigSecretName}, &sec); err != nil {
		t.Fatalf("openclaw-config not created: %v", err)
	}
	if strings.Contains(string(sec.Data["openclaw.json"]), "deepseek-v4-flash") {
		t.Errorf("model with missing credential should be skipped")
	}
}
