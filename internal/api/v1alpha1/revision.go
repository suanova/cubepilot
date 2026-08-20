package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// specRevision returns a short, immutable content fingerprint of a spec
// (design §3.5: templates and capabilities are versioned; TaskRuns record the
// revision actually used at run time for audit and rollback).
//
// A content hash is used instead of metadata.generation because it is
// deterministic across object re-creation and lets an auditor compare "what
// ran" across runs without depending on object lifecycle. The hash covers the
// spec only (not status/metadata), so a status update never changes the
// revision.
func specRevision(spec any) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(b)
	// 12 hex chars = 48 bits — collision-improbable for audit identification
	// while staying readable in kubectl output and report headers.
	return hex.EncodeToString(sum[:])[:12]
}
