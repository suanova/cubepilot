// Package grants stores the learned "allow always" grants for a user: rules
// the agent proposed, the user approved in chat, and the platform recorded so
// the command auto-passes from then on (issue #185).
//
// Grants are recorded state, not desired state. They are kept out of
// AgentInstance.spec so that a machine write can no longer flip the "the
// instance owns its list" sentinel, and because a lost grant is fail-closed:
// the command simply asks again. Storage is a per-user ConfigMap whose only
// writer is the API server, which keeps the whole-object-overwrite failure
// mode out of a field the controller also writes.
package grants

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// MaxGrants bounds a user's learned grants. Growth is driven by human clicks
// rather than by the agent, so this is a safety net against a pathological
// loop, not a working limit -- it has to sit far above what a heavy user
// accumulates in a year (order of 300) or it silently evicts grants people
// still rely on.
//
// It bounds the entry count, not the payload. The per-entry bounds below
// multiplied by 1000 is several MiB, well past the ~1 MiB ConfigMap ceiling, so
// Add also evicts against maxDataBytes. Sizing: a typical entry is roughly 250
// bytes (32-char key plus the JSON value), so 1000 of them is about a quarter of
// the ceiling; maxCommandBytes, maxRuleBytes and maxDataBytes are what keep the
// arithmetic honest at the extremes.
const (
	MaxGrants = 1000
	// maxCommandBytes bounds the stored command text, which is display-only and
	// plays no part in matching. Without a bound a single `bash -c` with a long
	// heredoc could push the ConfigMap past the 1 MiB API-server ceiling, and
	// the failure would not be confined to that entry: every later Add for that
	// user would fail too.
	maxCommandBytes = 512
	// maxRuleBytes bounds Pattern and ArgPattern together. A rule over this is
	// refused rather than truncated: ArgPattern is a regular expression the
	// gateway matches, so a shortened one silently matches something different,
	// and cutting ^...$ mid-pattern breaks the anchors outright. Pattern is a
	// bare command name and unbounded in principle for the same reason.
	//
	// The refusal is visible, not silent: Add reports it, the API answers
	// allowlisted: false and the Portal tells the user the command was not
	// added.
	maxRuleBytes = 4096
	// maxDataBytes bounds the whole serialized Data map, well under the 1 MiB
	// ConfigMap ceiling and above what a thousand typical entries need. The
	// entry-count cap does not bound the payload, so without this a user can
	// pass the ceiling far below MaxGrants -- and an oversized ConfigMap is
	// rejected for every later write of that user's, not just the current one,
	// which is why the store evicts to fit before calling Update rather than
	// letting the API server decide.
	maxDataBytes = 512 * 1024
)

// The data keys of the grants ConfigMap are opaque digests; the values are
// Records. One key per grant makes an add a single-key write that cannot lose
// a concurrent add of a different grant.
const managedByLabel = "cubepilot-grants"

// Record is one stored grant.
type Record struct {
	Pattern    string `json:"pattern"`
	ArgPattern string `json:"argPattern,omitempty"`
	// Command is the invocation that produced the grant, kept for the UI so a
	// learned rule can be shown as something a human recognises. It is display
	// only -- matching uses Pattern and ArgPattern -- and is truncated by
	// maxCommandBytes, with a trailing ellipsis when it was.
	Command   string    `json:"command,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Rule is the public-API allowlist rule this grant contributes.
func (r Record) Rule() v1alpha1.AllowlistRule {
	return v1alpha1.AllowlistRule{Pattern: r.Pattern, ArgPattern: r.ArgPattern}
}

// Store reads and writes the per-user grants ConfigMap. max and maxBytes are
// fields rather than the constants directly so the eviction tests can drive a
// small cap and a small budget instead of writing a thousand entries or a few
// hundred kilobytes.
type Store struct {
	cr       client.Client
	ns       string
	max      int
	maxBytes int
}

// New returns a Store backed by cr in namespace ns.
func New(cr client.Client, namespace string) *Store {
	return &Store{cr: cr, ns: namespace, max: MaxGrants, maxBytes: maxDataBytes}
}

// Name is the grants ConfigMap name for a user.
func (s *Store) Name(user string) string {
	return k8s.ResourceName("cubepilot-grants", user)
}

// Key is the stable data key for a rule: a hex digest of the same
// pattern|argPattern identity allowlist.Merge dedups on.
func Key(r v1alpha1.AllowlistRule) string {
	sum := sha256.Sum256([]byte(r.Pattern + "|" + r.ArgPattern))
	return hex.EncodeToString(sum[:16])
}

// List returns the user's grants, oldest first. A missing ConfigMap is an
// empty list rather than an error: most instances never record one.
func (s *Store) List(ctx context.Context, user string) ([]Record, error) {
	var cm corev1.ConfigMap
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: s.Name(user)}, &cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Record, 0, len(cm.Data))
	for _, raw := range cm.Data {
		var rec Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.Pattern == "" {
			// A malformed value is skipped rather than failing the resolve: a
			// dropped grant asks again, which is the safe direction.
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Add records a grant. It is idempotent on (pattern, argPattern): recording
// the same rule again keeps the original CreatedAt, so the cap evicts by
// first-seen order rather than being kept alive by repeats.
//
// A rule too large for maxRuleBytes is refused. Only the display-only command
// text is ever shortened; the matching fields are not, because a truncated
// pattern is a different pattern.
func (s *Store) Add(ctx context.Context, user string, r v1alpha1.AllowlistRule, command string, now time.Time) error {
	if r.Pattern == "" {
		return nil
	}
	if len(r.Pattern)+len(r.ArgPattern) > maxRuleBytes {
		return fmt.Errorf("grant rule for %s is %d bytes, over the %d byte bound",
			user, len(r.Pattern)+len(r.ArgPattern), maxRuleBytes)
	}
	raw, err := json.Marshal(Record{
		Pattern:    r.Pattern,
		ArgPattern: r.ArgPattern,
		Command:    truncate(command, maxCommandBytes),
		CreatedAt:  now.UTC(),
	})
	if err != nil {
		return err
	}
	key := Key(r)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.ensure(ctx, user)
		if err != nil {
			return err
		}
		if _, exists := cm.Data[key]; exists {
			return nil
		}
		cm.Data[key] = string(raw)
		if !evict(cm, s.max, s.maxBytes) {
			// Refuse rather than write an object the API server will reject: an
			// oversized ConfigMap fails every later write for this user, not
			// just this one. This is not a conflict, so RetryOnConflict surfaces
			// it instead of looping. The per-rule bound above makes it
			// unreachable for entries this build wrote; it is the backstop for a
			// ConfigMap written by an older build.
			return fmt.Errorf("grants for %s would exceed the %d byte payload budget", user, s.maxBytes)
		}
		return s.cr.Update(ctx, cm)
	})
}

// truncate bounds s to about n bytes, cutting on a byte boundary. The value is
// display-only -- matching uses Pattern and ArgPattern -- so a split rune at the
// cut is acceptable and not worth the extra code to avoid. The ellipsis is what
// matters: a silently shortened command reads as a complete one, and the user
// would be looking at a rule whose text they cannot trust.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Remove revokes a grant. It is idempotent: a missing ConfigMap, or a key that
// is not there, is a no-op rather than an error, so a double-click or a stale
// UI does not surface a failure.
func (s *Store) Remove(ctx context.Context, user string, r v1alpha1.AllowlistRule) error {
	if r.Pattern == "" {
		return nil
	}
	key := Key(r)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm corev1.ConfigMap
		err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: s.Name(user)}, &cm)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if _, exists := cm.Data[key]; !exists {
			return nil
		}
		delete(cm.Data, key)
		if len(cm.Data) == 0 {
			return s.cr.Delete(ctx, &cm)
		}
		return s.cr.Update(ctx, &cm)
	})
}

// ensure returns the user's grants ConfigMap, creating it when absent.
//
// The owner reference ties the ConfigMap to the instance, so it is
// garbage-collected with the instance. Kubernetes resolves that relationship by
// UID, so the reference is built from the instance object, not from its name: a
// reference with an empty UID is dangling, and the dependent is then liable to
// be deleted rather than collected with its owner. With no instance to point at
// (not provisioned yet, or already gone) the ConfigMap is created without an
// owner reference instead of with a broken one.
func (s *Store) ensure(ctx context.Context, user string) (*corev1.ConfigMap, error) {
	name := s.Name(user)
	var cm corev1.ConfigMap
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm)
	if err == nil {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		return &cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	var refs []metav1.OwnerReference
	var inst v1alpha1.AgentInstance
	instName := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	err = s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: instName}, &inst)
	switch {
	case err == nil:
		refs = []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "AgentInstance",
			Name:       inst.Name,
			UID:        inst.UID,
		}}
	case apierrors.IsNotFound(err):
		// No owner to reference.
	default:
		return nil, err
	}

	cm = corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       s.ns,
			Labels:          map[string]string{"app.kubernetes.io/managed-by": managedByLabel},
			OwnerReferences: refs,
		},
		Data: map[string]string{},
	}
	if err := s.cr.Create(ctx, &cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Lost a create race: re-read and use the existing object.
		if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: name}, &cm); err != nil {
			return nil, err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
	}
	return &cm, nil
}

// dataSize is the serialized size of a ConfigMap's Data, which is the bulk of
// what the API server counts against its ~1 MiB object ceiling.
func dataSize(cm *corev1.ConfigMap) int {
	n := 0
	for k, v := range cm.Data {
		n += len(k) + len(v)
	}
	return n
}

// evict bounds a ConfigMap's payload: it drops the oldest entries until at most
// max remain and the serialized Data fits budget. It reports whether the payload
// fits afterwards; false means the surviving entries alone are over budget,
// which the Add-time rule bound makes unreachable for entries this build wrote.
//
// The count and the size are bounded together rather than in two passes because
// both order the entries the same way, by CreatedAt.
func evict(cm *corev1.ConfigMap, max, budget int) bool {
	type keyed struct {
		key string
		at  time.Time
	}
	all := make([]keyed, 0, len(cm.Data))
	for k, raw := range cm.Data {
		var rec Record
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			// Undecodable entries can never be ordered; drop them first.
			delete(cm.Data, k)
			continue
		}
		all = append(all, keyed{key: k, at: rec.CreatedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all) && (len(cm.Data) > max || dataSize(cm) > budget); i++ {
		delete(cm.Data, all[i].key)
	}
	return len(cm.Data) <= max && dataSize(cm) <= budget
}
