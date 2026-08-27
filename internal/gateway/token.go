package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/k8s"
)

// EnsureGatewayToken reads the gatewayToken from the openclaw-config Secret
// (k8s.ConfigSecretName), generating and persisting one if absent. Idempotent
// and concurrency-safe: callers race to create; the loser re-reads the
// winner's token.
func EnsureGatewayToken(ctx context.Context, cl client.Client, ns string) (string, error) {
	key := types.NamespacedName{Namespace: ns, Name: k8s.ConfigSecretName}
	var sec corev1.Secret
	err := cl.Get(ctx, key, &sec)
	if err == nil {
		if tok := string(sec.Data["gatewayToken"]); tok != "" {
			return tok, nil
		}
		// Secret exists but the token is empty/missing: repair it so the state
		// self-heals instead of persisting an empty auth token.
		token, err := randomToken()
		if err != nil {
			return "", err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data["gatewayToken"] = []byte(token)
		if err := cl.Update(ctx, &sec); err != nil {
			return "", err
		}
		return token, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.ConfigSecretName, Namespace: ns},
		Data:       map[string][]byte{"gatewayToken": []byte(token)},
	}
	if err := cl.Create(ctx, &sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost the create race: reuse the winner's persisted token.
			if err := cl.Get(ctx, key, &sec); err != nil {
				return "", err
			}
			return string(sec.Data["gatewayToken"]), nil
		}
		return "", err
	}
	return token, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
