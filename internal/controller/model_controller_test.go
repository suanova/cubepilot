package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
)

// modelTestClient builds a fake client with the platform types + corev1 and
// the status subresource enabled for Model (fake client ignores status writes
// otherwise).
func modelTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Model{}).
		WithObjects(objs...).
		Build()
}

// TestModelReconcilerExternalProbe verifies that an external model with a
// reachable endpoint + credential becomes Available (design §3.3).
func TestModelReconcilerExternalProbe(t *testing.T) {
	// Reachable endpoint (returns 200 on GET /models).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("probe path = %s, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cred-llm-org", Namespace: "cubepilot"},
		Data:       map[string][]byte{"apiKey": []byte("sk-test")},
	}
	cl := modelTestClient(t, secret)
	r := &ModelReconciler{Client: cl, Cfg: config.Config{Namespace: "cubepilot"}}

	model := &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "external-model"},
		Spec: v1alpha1.ModelSpec{
			Provider:      v1alpha1.ModelProviderExternal,
			Endpoint:      srv.URL,
			CredentialRef: "cubepilot/cred-llm-org",
		},
	}
	if err := cl.Create(context.Background(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "external-model"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.Model
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "external-model"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.ModelAvailable {
		t.Errorf("phase = %q, want Available (message %q)", got.Status.Phase, got.Status.Message)
	}
}

// TestModelReconcilerUnreachable verifies the Unreachable path: a bad
// endpoint must not be offered as Available.
func TestModelReconcilerUnreachable(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cred-llm-org", Namespace: "cubepilot"},
		Data:       map[string][]byte{"apiKey": []byte("sk-test")},
	}
	cl := modelTestClient(t, secret)
	r := &ModelReconciler{Client: cl, Cfg: config.Config{Namespace: "cubepilot"}}

	model := &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-model"},
		Spec: v1alpha1.ModelSpec{
			Provider:      v1alpha1.ModelProviderExternal,
			Endpoint:      "http://127.0.0.1:1/v1", // nothing listens here
			CredentialRef: "cred-llm-org",
		},
	}
	if err := cl.Create(context.Background(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "bad-model"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.Model
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "bad-model"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.ModelUnreachable {
		t.Errorf("phase = %q, want Unreachable", got.Status.Phase)
	}
	if got.Status.Message == "" {
		t.Error("message should carry the probe failure reason")
	}
}

// TestModelReconcilerPlatform verifies platform-provided models are always
// Available without probing (no endpoint, no credential).
func TestModelReconcilerPlatform(t *testing.T) {
	cl := modelTestClient(t)
	r := &ModelReconciler{Client: cl, Cfg: config.Config{Namespace: "cubepilot"}}

	model := &v1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "deepseek-v4-flash"},
		Spec: v1alpha1.ModelSpec{
			Provider: v1alpha1.ModelProviderPlatform,
		},
	}
	if err := cl.Create(context.Background(), model); err != nil {
		t.Fatalf("create model: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "deepseek-v4-flash"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.Model
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "deepseek-v4-flash"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.ModelAvailable {
		t.Errorf("phase = %q, want Available", got.Status.Phase)
	}
}
