package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// countingClient records the writes issued through it, so a test can assert
// that an unchanged reconcile issues none.
type countingClient struct {
	client.Client
	creates int
	updates int
}

func (c *countingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates++
	return c.Client.Create(ctx, obj, opts...)
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates++
	return c.Client.Update(ctx, obj, opts...)
}

// alwaysStaleClient never sees key and never wins the create for it, standing
// in for the production client: a manager client reads Secrets through the
// controller-runtime cache while writes go straight to the API server, so an
// object that exists can be missing from every read this client makes.
type alwaysStaleClient struct {
	client.Client
	key types.NamespacedName
}

func (c *alwaysStaleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == c.key {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *alwaysStaleClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
}

// staleReadAtClient reports the nth read of key as not found while the object
// is really there, and rejects every create. It stands in for a manager client
// whose cache is behind on the object at that one read: the way out is the
// caller's uncached reader, not this client.
type staleReadAtClient struct {
	client.Client
	key   types.NamespacedName
	at    int
	reads int
}

func (c *staleReadAtClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == c.key {
		c.reads++
		if c.reads == c.at {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *staleReadAtClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, obj.GetName())
}

func TestEnsureSecretDataCreates(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	key := types.NamespacedName{Namespace: "cubepilot", Name: "openclaw-config"}
	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{"cubepilot/builtin": "true"}},
		Data:       map[string][]byte{"gatewayToken": []byte("tok"), "openclaw.json": []byte("{}")},
	}

	if err := ensureSecretData(context.Background(), cl, cl, want); err != nil {
		t.Fatalf("ensureSecretData: %v", err)
	}

	var got corev1.Secret
	if err := cl.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if string(got.Data["gatewayToken"]) != "tok" || string(got.Data["openclaw.json"]) != "{}" {
		t.Errorf("data = %v, want both keys", got.Data)
	}
	// The caller's metadata survives the create: the per-user kubeconfig Secret
	// is found again by its label.
	if got.Labels["cubepilot/builtin"] != "true" {
		t.Errorf("labels = %v, want the caller's", got.Labels)
	}
}

func TestEnsureSecretDataWritesNothingWhenUnchanged(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "cubepilot", Name: "openclaw-config"}
	want := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Data:       map[string][]byte{"gatewayToken": []byte("tok"), "openclaw.json": []byte("{}")},
		}
	}
	if err := ensureSecretData(ctx, cl, cl, want()); err != nil {
		t.Fatalf("ensureSecretData: %v", err)
	}

	counted := &countingClient{Client: cl}
	if err := ensureSecretData(ctx, counted, cl, want()); err != nil {
		t.Fatalf("ensureSecretData #2: %v", err)
	}
	if counted.creates != 0 || counted.updates != 0 {
		t.Errorf("unchanged data issued %d creates and %d updates, want none", counted.creates, counted.updates)
	}
}

func TestEnsureSecretDataUpdatesChangedData(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "openclaw-config", Namespace: "cubepilot"},
		Data:       map[string][]byte{"gatewayToken": []byte("tok"), "openclaw.json": []byte(`{"old":true}`)},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(existing).Build()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "cubepilot", Name: "openclaw-config"}

	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{"gatewayToken": []byte("tok"), "openclaw.json": []byte(`{"new":true}`)},
	}
	if err := ensureSecretData(ctx, cl, cl, want); err != nil {
		t.Fatalf("ensureSecretData: %v", err)
	}

	var got corev1.Secret
	if err := cl.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Data["openclaw.json"]) != `{"new":true}` {
		t.Errorf("openclaw.json = %q, want the new render", got.Data["openclaw.json"])
	}
}

// TestEnsureSecretDataConvergesAfterLostCreate pins the create race: the caller's
// client misses the Secret on every read and loses the create, so the fallback
// read has to go past that client to reach the object that is really there --
// otherwise the desired data is dropped for a later reconcile to write.
func TestEnsureSecretDataConvergesAfterLostCreate(t *testing.T) {
	winner := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "openclaw-config", Namespace: "cubepilot"},
		Data:       map[string][]byte{"gatewayToken": []byte("winner")},
	}
	base := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(winner).Build()
	key := types.NamespacedName{Namespace: "cubepilot", Name: "openclaw-config"}
	cl := &alwaysStaleClient{Client: base, key: key}

	want := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       map[string][]byte{"gatewayToken": []byte("winner"), "openclaw.json": []byte("{}")},
	}
	if err := ensureSecretData(context.Background(), cl, base, want); err != nil {
		t.Fatalf("ensureSecretData: %v", err)
	}

	var got corev1.Secret
	if err := base.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Data["openclaw.json"]) != "{}" {
		t.Errorf("openclaw.json = %q, want the desired render written onto the winner", got.Data["openclaw.json"])
	}
	if string(got.Data["gatewayToken"]) != "winner" {
		t.Errorf("gatewayToken = %q, want the winner's token kept", got.Data["gatewayToken"])
	}
}
