package logging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/go-logr/logr"
)

// TestSecretBodyIsSuppressedAtDefaultLevel is the regression test for the leak
// this branch was opened to fix: client-go's V(8) request/response body dump
// (rest/request.go logBody) hex-dumps whole API bodies, Secret bodies
// included, and without a level gate that reaches the pod log unconditionally.
// It drives the real client-go path -- an httptest server standing in for the
// API server, and the sink's Enabled reached the way client-go actually reads
// it, through klog.FromContext -- rather than asserting the gate in isolation,
// so a regression in how the level is wired through would be caught here even
// if the gate itself still worked.
func TestSecretBodyIsSuppressedAtDefaultLevel(t *testing.T) {
	const ns = "default"
	const name = "test-secret"
	const secretValue = "s3cr3t-value-should-not-leak"
	// corev1.Secret.Data is []byte and marshals to base64 on the wire, so the
	// recognisable needle in the JSON response body is the base64 form, not
	// the plaintext.
	wireValue := base64.StdEncoding.EncodeToString([]byte(secretValue))

	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"password": []byte(secretValue)},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(secret)
	}))
	defer srv.Close()

	getSecret := func(t *testing.T, level int) string {
		t.Helper()
		var buf bytes.Buffer
		logger := logr.New(newSink(&buf, level))

		clientset, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
		if err != nil {
			t.Fatalf("NewForConfig: %v", err)
		}

		ctx := klog.NewContext(context.Background(), logger)
		got, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(got.Data["password"]) != secretValue {
			t.Fatalf("round trip changed the secret value: got %q", got.Data["password"])
		}
		return buf.String()
	}

	t.Run("level 0 suppresses the body", func(t *testing.T) {
		got := getSecret(t, 0)
		if strings.Contains(got, "Response Body") {
			t.Errorf("Response Body line reached the sink at level 0: %q", got)
		}
		if strings.Contains(got, wireValue) {
			t.Errorf("secret value reached the sink at level 0: %q", got)
		}
	})

	t.Run("level 8 is the documented consequence of raising it", func(t *testing.T) {
		got := getSecret(t, 8)
		if !strings.Contains(got, "Response Body") {
			t.Errorf("Response Body line missing at level 8: %q", got)
		}
		if !strings.Contains(got, wireValue) {
			t.Errorf("secret value missing from the body dump at level 8: %q", got)
		}
	})
}
