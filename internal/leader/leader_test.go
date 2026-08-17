package leader

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeTime returns a metav1.NowMicro-based timestamp helper.
func leaseNow() *metav1.MicroTime {
	now := metav1.NowMicro()
	return &now
}

func TestSingleReplicaAlwaysLeader(t *testing.T) {
	e := New(nil, "ns", "cubepilot-test", 1)
	if !e.IsLeader() {
		t.Fatal("single-replica should always be leader")
	}
}

func TestAcquireCreatesLeaseAndLeads(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "cubepilot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := New(client, ns, "cubepilot-test", 2)
	if e.IsLeader() {
		t.Fatal("multi-replica must not start as leader")
	}
	go e.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if e.IsLeader() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no leadership acquired within 5s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	lease, err := client.CoordinationV1().Leases(ns).Get(ctx, "cubepilot-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lease not created: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Fatal("lease holder missing")
	}
}

func TestNoTakeoverWhileHeld(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "cubepilot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A live lease held by another holder, renew time in the past 5s (still
	// within the 15s duration): the elector must stay standby.
	fresh := time.Now().Add(-5 * time.Second)
	_, err := client.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-test", Namespace: ns},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       strPtr("other-holder"),
			LeaseDurationSeconds: int32Ptr(15),
			AcquireTime:          &metav1.MicroTime{Time: fresh},
			RenewTime:            &metav1.MicroTime{Time: fresh},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("precreate lease: %v", err)
	}

	e := New(client, ns, "cubepilot-test", 2)
	go e.Run(ctx)

	time.Sleep(1500 * time.Millisecond) // several ticks
	if e.IsLeader() {
		t.Fatal("must not take over a live lease held by another holder")
	}
}

func TestTakeoverAfterExpiry(t *testing.T) {
	client := fake.NewSimpleClientset()
	ns := "cubepilot"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pre-create a lease held by a dead holder with an expired renew time.
	expired := time.Now().Add(-1 * time.Hour)
	_, err := client.CoordinationV1().Leases(ns).Create(ctx, &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "cubepilot-test", Namespace: ns},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       strPtr("dead-holder"),
			LeaseDurationSeconds: int32Ptr(15),
			AcquireTime:          &metav1.MicroTime{Time: expired},
			RenewTime:            &metav1.MicroTime{Time: expired},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("precreate lease: %v", err)
	}

	e := New(client, ns, "cubepilot-test", 2)
	go e.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if e.IsLeader() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired lease not taken over within 5s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	lease, _ := client.CoordinationV1().Leases(ns).Get(ctx, "cubepilot-test", metav1.GetOptions{})
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "dead-holder" {
		t.Fatalf("takeover failed: holder=%v", lease.Spec.HolderIdentity)
	}
}

func strPtr(s string) *string      { return &s }
func int32Ptr(v int32) *int32      { return &v }
func TestLeaseResourceName(t *testing.T) {
	if got := LeaseResourceName("instance-manager"); got != "cubepilot-instance-manager" {
		t.Fatalf("want cubepilot-instance-manager, got %s", got)
	}
}

var _ = leaseNow // keep helper referenced
