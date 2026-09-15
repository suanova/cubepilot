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

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

func TestOpenClawConfigReconcile(t *testing.T) {
	scheme := testScheme(t)
	builtin := BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
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
	if !strings.Contains(jsonData, `"platform/deepseek-v4-flash"`) {
		t.Errorf("openclaw.json missing primary ref: %s", jsonData)
	}
	expectedID := "/" + k8s.EnvNameForProvider(BuiltinProviderName)
	if !strings.Contains(jsonData, `"id": "`+expectedID+`"`) {
		t.Errorf("openclaw.json apiKey should be a file SecretRef (cubepilot-keys %s): %s", expectedID, jsonData)
	}
	if strings.Contains(jsonData, "sk-real") {
		t.Errorf("openclaw.json must not contain the literal credential: %s", jsonData)
	}
	if tok := string(sec.Data["gatewayToken"]); len(tok) != 64 {
		t.Errorf("gatewayToken length = %d, want 64", len(tok))
	}
}

// TestOpenClawConfigReconcileSkipsDuplicateProvider pins the cross-template
// merge rule. A provider name is unique within its own template (+listMapKey=name
// scopes the key to the list), but every template in the namespace is merged
// into one gateway config. A name defined twice would let the later
// models.providers entry overwrite the earlier one while the allowlist kept refs
// from both -- routing one template's model to another template's endpoint and
// credential. Name order settles it: the first provider to claim a name wins and
// a later duplicate is skipped whole, models included.
func TestOpenClawConfigReconcileSkipsDuplicateProvider(t *testing.T) {
	scheme := testScheme(t)
	// The duplicate is the alphabetically later template, so the winner is
	// settled by the merge's name sort rather than by the order the templates
	// come back from the list.
	loser := providerTemplate("zzz-second", "http://second.example/v1", "m-two")
	winner := providerTemplate("aaa-first", "http://first.example/v1", "m-one")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(loser, winner).Build()
	r := &OpenClawConfigReconciler{Client: cl, Scheme: scheme, Cfg: config.Config{Namespace: "cubepilot"}}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var sec corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: k8s.ConfigSecretName}, &sec); err != nil {
		t.Fatalf("openclaw-config not created: %v", err)
	}
	jsonData := string(sec.Data["openclaw.json"])
	if !strings.Contains(jsonData, "http://first.example/v1") || !strings.Contains(jsonData, "vllm/m-one") {
		t.Errorf("the first template in name order must own the provider name: %s", jsonData)
	}
	if strings.Contains(jsonData, "http://second.example/v1") {
		t.Errorf("the duplicate provider's endpoint must not be rendered: %s", jsonData)
	}
	if strings.Contains(jsonData, "m-two") {
		t.Errorf("the duplicate provider's model must not reach the config or the allowlist: %s", jsonData)
	}
}

// providerTemplate is a minimal agent template declaring one provider named
// vllm, for the merge tests.
func providerTemplate(name, endpoint, modelID string) *v1alpha1.AgentTemplate {
	return &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cubepilot"},
		Spec: v1alpha1.AgentTemplateSpec{
			Providers: []v1alpha1.TemplateProviderSpec{
				{Name: "vllm", Endpoint: endpoint, Models: []string{modelID}},
			},
		},
	}
}

func TestOpenClawConfigReconcileSkipsMissingCredential(t *testing.T) {
	scheme := testScheme(t)
	builtin := BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash") // references cubepilot-llm, which is absent
	builtin.Namespace = "cubepilot"
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
