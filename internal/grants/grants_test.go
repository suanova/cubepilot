package grants

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

func testStore(t *testing.T, objs ...client.Object) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	return New(fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(), "cubepilot")
}

// testStoreWithMax builds a Store with an overridden cap, so the eviction test
// drives three entries instead of a thousand.
func testStoreWithMax(t *testing.T, max int, objs ...client.Object) *Store {
	t.Helper()
	s := testStore(t, objs...)
	s.max = max
	return s
}

// testStoreWithBudget builds a Store with a payload budget small enough to
// reach in a handful of writes, so the size eviction is exercised without
// serializing hundreds of kilobytes.
func testStoreWithBudget(t *testing.T, maxBytes int, objs ...client.Object) *Store {
	t.Helper()
	s := testStore(t, objs...)
	s.maxBytes = maxBytes
	return s
}

func TestListMissingConfigMapIsEmpty(t *testing.T) {
	s := testStore(t)
	got, err := s.List(context.Background(), "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d grants, want 0", len(got))
	}
}

func TestAddThenList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	rule := v1alpha1.AllowlistRule{Pattern: "kubectl", ArgPattern: `^get pods$`}

	if err := s.Add(ctx, "alice", rule, "kubectl get pods", now); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	if got[0].Pattern != "kubectl" || got[0].ArgPattern != `^get pods$` {
		t.Errorf("round trip lost the rule: %+v", got[0])
	}
	if got[0].Command != "kubectl get pods" {
		t.Errorf("Command = %q", got[0].Command)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", got[0].CreatedAt, now)
	}
}

// TestGrantListsArePerUser: Name has to key the ConfigMap on the user, or two
// users share one allowlist. That would be both an allowlist leak (one user's
// approved rule auto-passing for another) and a cross-user write (one user's
// Add evicting another's grants), so it is pinned rather than left to the
// implementation to remember.
func TestGrantListsArePerUser(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	alice := v1alpha1.AllowlistRule{Pattern: "kubectl", ArgPattern: `^get pods$`}

	if err := s.Add(ctx, "alice", alice, "kubectl get pods", time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	bobGrants, err := s.List(ctx, "bob")
	if err != nil {
		t.Fatalf("List bob: %v", err)
	}
	if len(bobGrants) != 0 {
		t.Errorf("bob sees %d of alice's grants: %+v", len(bobGrants), bobGrants)
	}
	aliceGrants, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List alice: %v", err)
	}
	if len(aliceGrants) != 1 || aliceGrants[0].Pattern != alice.Pattern {
		t.Errorf("alice's grants = %+v, want her own", aliceGrants)
	}
}

// TestAddIsIdempotent pins the dedup identity to pattern|argPattern: the same
// command approved twice must not grow the list, and must keep the original
// CreatedAt so the cap evicts by first-seen order.
func TestAddIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rule := v1alpha1.AllowlistRule{Pattern: "helm"}
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

	if err := s.Add(ctx, "alice", rule, "helm install x", first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if err := s.Add(ctx, "alice", rule, "helm install y", second); err != nil {
		t.Fatalf("Add second: %v", err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	if !got[0].CreatedAt.Equal(first) {
		t.Errorf("CreatedAt = %v, want the original %v", got[0].CreatedAt, first)
	}
}

// TestAddEvictsOldestPastCap: growth is human-driven, so the cap is a safety
// net against a loop rather than a working limit. The store's cap is
// overridden to 3 so this exercises the eviction path in three writes.
func TestAddEvictsOldestPastCap(t *testing.T) {
	s := testStoreWithMax(t, 3)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		rule := v1alpha1.AllowlistRule{Pattern: "cmd", ArgPattern: "arg-" + strconv.Itoa(i)}
		if err := s.Add(ctx, "alice", rule, "", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	// One more pushes past the cap.
	newest := v1alpha1.AllowlistRule{Pattern: "newest"}
	if err := s.Add(ctx, "alice", newest, "", base.Add(time.Hour)); err != nil {
		t.Fatalf("Add newest: %v", err)
	}

	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d grants, want 3", len(got))
	}
	// The very first rule added must be gone, the newest kept.
	for _, r := range got {
		if r.Pattern == "cmd" && r.ArgPattern == "arg-0" {
			t.Error("oldest grant was not evicted")
		}
	}
	if got[len(got)-1].Pattern != "newest" {
		t.Errorf("newest grant missing; last = %+v", got[len(got)-1])
	}
}

// TestAddTruncatesLongCommand: the command text is display-only, and leaving it
// unbounded would make the per-entry size -- and so the MaxGrants arithmetic --
// meaningless.
func TestAddTruncatesLongCommand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	long := strings.Repeat("a", maxCommandBytes*2)
	if err := s.Add(ctx, "alice", v1alpha1.AllowlistRule{Pattern: "bash"}, long, time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	// Truncated to the cap, with a visible marker so the UI can tell the text
	// is incomplete.
	want := maxCommandBytes + len("...")
	if len(got[0].Command) != want {
		t.Errorf("Command length = %d, want %d", len(got[0].Command), want)
	}
	if !strings.HasSuffix(got[0].Command, "...") {
		t.Errorf("truncated command has no marker: %q", got[0].Command)
	}
}

// TestRemoveDropsTheGrantAndIsIdempotent: revoking a learned grant must work
// from the UI, and revoking one that is already gone must not error.
func TestRemoveDropsTheGrantAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	keep := v1alpha1.AllowlistRule{Pattern: "terraform"}
	drop := v1alpha1.AllowlistRule{Pattern: "helm"}
	if err := s.Add(ctx, "alice", keep, "", time.Now()); err != nil {
		t.Fatalf("Add keep: %v", err)
	}
	if err := s.Add(ctx, "alice", drop, "", time.Now()); err != nil {
		t.Fatalf("Add drop: %v", err)
	}

	if err := s.Remove(ctx, "alice", drop); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.Remove(ctx, "alice", drop); err != nil {
		t.Fatalf("Remove again should be a no-op: %v", err)
	}
	// Removing from a user with no ConfigMap at all is also a no-op.
	if err := s.Remove(ctx, "bob", drop); err != nil {
		t.Fatalf("Remove for a user without grants: %v", err)
	}

	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != "terraform" {
		t.Fatalf("grants = %+v, want only terraform", got)
	}
}

// TestRemoveKeepsTheEmptyConfigMap: removing the last grant must leave the
// ConfigMap in place, not delete it. Deleting it would be an unserialized
// get-then-delete that RetryOnConflict does not cover -- neither failure is a
// conflict -- so a concurrent Add could either fail with a spurious NotFound or
// lose the grant it just wrote. An empty ConfigMap costs nothing and the owner
// reference collects it with the instance.
func TestRemoveKeepsTheEmptyConfigMap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	only := v1alpha1.AllowlistRule{Pattern: "terraform"}
	if err := s.Add(ctx, "alice", only, "", time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := s.Remove(ctx, "alice", only); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var cm corev1.ConfigMap
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: s.Name("alice")}, &cm); err != nil {
		t.Fatalf("ConfigMap should survive the last revoke: %v", err)
	}
	if len(cm.Data) != 0 {
		t.Errorf("Data = %+v, want empty", cm.Data)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("grants = %+v, want none", got)
	}
}

// TestAddSetsOwnerReference: the ConfigMap is garbage-collected with the
// instance it belongs to. Garbage collection matches an owner by UID, so the
// reference is populated from the instance object -- a name-only reference has
// an empty UID, which is dangling and leaves the ConfigMap liable to be deleted
// rather than collected with its owner.
func TestAddSetsOwnerReference(t *testing.T) {
	inst := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8s.InstanceName("alice", v1alpha1.DefaultAgentName),
			Namespace: "cubepilot",
			UID:       types.UID("11111111-2222-3333-4444-555555555555"),
		},
	}
	s := testStore(t, inst)
	ctx := context.Background()
	if err := s.Add(ctx, "alice", v1alpha1.AllowlistRule{Pattern: "helm"}, "", time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var cm corev1.ConfigMap
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: s.Name("alice")}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if got := cm.Name; got != k8s.ResourceName("cubepilot-grants", "alice") {
		t.Errorf("ConfigMap name = %q", got)
	}
	if len(cm.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references, want 1", len(cm.OwnerReferences))
	}
	ref := cm.OwnerReferences[0]
	if ref.Kind != "AgentInstance" || ref.Name != inst.Name {
		t.Errorf("owner reference = %+v", ref)
	}
	// Garbage collection resolves the owner kind through APIVersion, so a
	// reference without it is not a resolvable owner.
	if ref.APIVersion != v1alpha1.GroupVersion.String() {
		t.Errorf("owner reference APIVersion = %q, want %q", ref.APIVersion, v1alpha1.GroupVersion.String())
	}
	if ref.UID != inst.UID {
		t.Errorf("owner reference UID = %q, want %q", ref.UID, inst.UID)
	}
}

// TestAddWithoutInstanceSkipsOwnerReference: with no instance to own it, the
// ConfigMap is created unowned rather than with a dangling reference.
func TestAddWithoutInstanceSkipsOwnerReference(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Add(ctx, "alice", v1alpha1.AllowlistRule{Pattern: "helm"}, "", time.Now()); err != nil {
		t.Fatalf("Add: %v", err)
	}
	var cm corev1.ConfigMap
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: s.Name("alice")}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	if len(cm.OwnerReferences) != 0 {
		t.Errorf("owner references = %+v, want none", cm.OwnerReferences)
	}
}

// TestAddRefusesAnOversizedRule: a rule too large to store is refused, not
// truncated. ArgPattern is a regular expression the gateway matches, so a
// truncated one silently matches something different -- and cutting ^...$ mid
// pattern breaks the anchors outright. The refusal is visible: the API answers
// allowlisted: false and the Portal says the command was not added.
func TestAddRefusesAnOversizedRule(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rule := v1alpha1.AllowlistRule{Pattern: "bash", ArgPattern: strings.Repeat("a", maxRuleBytes+1)}
	if err := s.Add(ctx, "alice", rule, "", time.Now()); err == nil {
		t.Fatal("Add of an oversized rule should be refused")
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("refused rule was stored anyway: %+v", got)
	}
}

// TestAddEvictsToStayUnderTheSizeBudget: the entry cap bounds how many grants a
// user has, not how large they are -- a thousand entries at the per-entry
// maxima is megabytes -- so the store also drops the oldest entries until the
// serialized Data fits. Without that the API server rejects the ConfigMap, and
// the rejection is not confined to this write: every later write for that user
// fails too.
func TestAddEvictsToStayUnderTheSizeBudget(t *testing.T) {
	// The entries are deliberately tiny. The data keys are 32-byte digests, so
	// with small values the keys are a large share of each entry -- which is
	// what makes an eviction that stops counting them (see below) overshoot
	// visibly. A handful of adds reaches the budget.
	budget := 512
	s := testStoreWithBudget(t, budget)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	newest := "arg-5"

	for i := 0; i < 6; i++ {
		rule := v1alpha1.AllowlistRule{Pattern: "cmd", ArgPattern: "arg-" + strconv.Itoa(i)}
		if err := s.Add(ctx, "alice", rule, "", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	var cm corev1.ConfigMap
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: "cubepilot", Name: s.Name("alice")}, &cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	// Measured with json.Marshal rather than dataSize on purpose: dataSize is
	// the function evict bounds the payload with, so asserting against it would
	// keep passing if dataSize stopped counting what it should -- dropping
	// len(k), or returning 0 and disabling size eviction entirely -- which is
	// exactly the regression this test exists to catch.
	marshaled, err := json.Marshal(cm.Data)
	if err != nil {
		t.Fatalf("marshal Data: %v", err)
	}
	if got := len(marshaled); got > budget {
		t.Errorf("stored payload = %d bytes, over the %d budget", got, budget)
	}
	got, err := s.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) == 0 || got[len(got)-1].ArgPattern != newest {
		t.Errorf("newest grant evicted: %+v", got)
	}
}
