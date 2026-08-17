// Package leader implements a minimal Kubernetes Lease-based leader election
// (design doc §3.3: Instance Manager / 调度器 多副本 Active/Standby). When
// replicas == 1 no election runs and the process is leader by construction —
// single-replica deployments behave exactly as before.
package leader

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// Elector acquires and holds a Lease; IsLeader reports whether this process is
// currently the holder. The holder refreshes the lease every LeaseDuration/3;
// on loss (expiry or API error) it steps down and retries.
type Elector struct {
	client  kubernetes.Interface
	name    string // lease name, e.g. "cubepilot-instance-manager"
	ns      string
	holder  string // pod name or hostname fallback
	leaseMs int32
	ttl     time.Duration

	mu       chan struct{} // held while mutating isLeader
	isLeader bool
}

// New returns an Elector for the given lease name. replicas <= 1 disables
// election entirely (always leader).
func New(client kubernetes.Interface, ns, name string, replicas int) *Elector {
	e := &Elector{
		client:  client,
		name:    name,
		ns:      ns,
		holder:  holderID(),
		leaseMs: 15,
		ttl:     15 * time.Second,
		mu:      make(chan struct{}, 1),
	}
	if replicas <= 1 {
		e.isLeader = true
	}
	return e
}

func holderID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return fmt.Sprintf("cubepilot-%d", os.Getpid())
}

// IsLeader reports whether this process currently holds the lease. It is safe
// to call from multiple goroutines.
func (e *Elector) IsLeader() bool {
	e.mu <- struct{}{}
	defer func() { <-e.mu }()
	return e.isLeader
}

// Run drives the election loop until ctx is cancelled.
func (e *Elector) Run(ctx context.Context) {
	if e.isLeader && e.client == nil {
		return // single-replica mode: nothing to do
	}
	// Re-evaluate client nil guard for single replica with a client present.
	if e.client == nil {
		return
	}
	go func() {
		t := time.NewTicker(e.ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.acquire(ctx); err != nil {
					log.Printf("leader: %v", err)
				}
			}
		}
	}()
}

// acquire reads the lease and either renews (if we hold it), contends, or
// takes over an expired lease. It updates e.isLeader accordingly.
func (e *Elector) acquire(ctx context.Context) error {
	leases := e.client.CoordinationV1().Leases(e.ns)
	now := metav1.NowMicro()

	lease, err := leases.Get(ctx, e.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// Create with a short-held lock; if we lose the race, the next tick
		// re-reads and contends normally.
		_, err := leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: e.name, Namespace: e.ns},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(e.holder),
				LeaseDurationSeconds: ptr.To(e.leaseMs),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}, metav1.CreateOptions{})
		if err == nil {
			e.setLeader(true)
			return nil
		}
		if apierrors.IsAlreadyExists(err) {
			return e.acquire(ctx) // retry once via normal path
		}
		return err

	case err != nil:
		return fmt.Errorf("get lease %s: %w", e.name, err)
	}

	// We already hold it: renew.
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == e.holder {
		lease.Spec.RenewTime = &now
		if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
			e.setLeader(false) // lost the lease; step down
			return fmt.Errorf("renew lease: %w", err)
		}
		e.setLeader(true)
		return nil
	}

	// Someone else holds it. Take over only if expired.
	if lease.Spec.RenewTime != nil {
		expiry := lease.Spec.RenewTime.Add(time.Duration(ptr.Deref(lease.Spec.LeaseDurationSeconds, 15)) * time.Second)
		if now.Time.Before(expiry) {
			e.setLeader(false)
			return nil // not expired yet; standby
		}
	}
	// Expired: try to take over via update (optimistic concurrency on
	// resourceVersion).
	lease.Spec.HolderIdentity = ptr.To(e.holder)
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return nil // lost the race; re-read next tick
		}
		return fmt.Errorf("take over lease: %w", err)
	}
	e.setLeader(true)
	return nil
}

func (e *Elector) setLeader(v bool) {
	e.mu <- struct{}{}
	e.isLeader = v
	<-e.mu
}

// LeaseResourceName returns the standard leader-election lease name for a
// component, matching the naming convention used in deploy manifests.
func LeaseResourceName(component string) string {
	return "cubepilot-" + component
}
