package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/gateway"
)

// renderedProvider pulls one provider entry out of the openclaw.json the
// operator renders.
func renderedProvider(raw []byte, name string) (map[string]any, error) {
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	models, ok := cfg["models"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("models key missing")
	}
	providers, ok := models["providers"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("models.providers key missing")
	}
	prov, ok := providers[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("provider %q not rendered yet", name)
	}
	return prov, nil
}

// The LLM model lifecycle runs against the deployed API rather than a fake
// client on purpose: writing a credential Secret needs the API's
// ServiceAccount to hold create, update AND delete on secrets, and a chart
// that grants only create fails here and nowhere else. The handlers degrade
// gracefully -- a rotation returns 500, a delete leaves the Secret behind with
// a warning -- so neither a unit test nor `helm lint` sees it.
var _ = Describe("LLM model lifecycle", func() {
	ctx := context.Background()
	const (
		modelName  = "e2e-scratch-model"
		credName   = "llm-" + modelName
		modelPath  = "/api/llms/" + modelName
		remoteBase = "https://llm.example.com/v1"
	)

	// Every spec starts from a clean catalog entry. The Secret is removed
	// directly as well, because the API's own delete is one of the things under
	// test -- when it fails the Secret is exactly what it leaves behind.
	cleanup := func() {
		_, _, _ = fw.SendJSON(ctx, http.MethodDelete, fw.APIBase+modelPath, nil, nil)
		_ = fw.KubeClient.CoreV1().Secrets(fw.Namespace).Delete(ctx, credName, metav1.DeleteOptions{})
	}
	BeforeEach(cleanup)
	AfterEach(cleanup)

	credentialExists := func() bool {
		_, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, credName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		return err == nil
	}

	credentialValue := func() string {
		sec, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, credName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return string(sec.Data["apiKey"])
	}

	It("refuses a model with no credential that is not declared public", func() {
		body, code, err := fw.SendJSON(ctx, http.MethodPost, fw.APIBase+"/api/llms", map[string]any{
			"name":     modelName,
			"endpoint": remoteBase,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusBadRequest), "%v", body)
		Expect(credentialExists()).To(BeFalse(), "a rejected add must write nothing")
	})

	It("normalizes a full request URL and renders a keyless model", func() {
		body, code, err := fw.SendJSON(ctx, http.MethodPost, fw.APIBase+"/api/llms", map[string]any{
			"name":     modelName,
			"endpoint": remoteBase + "/chat/completions",
			"public":   true,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), "%v", body)
		// The SDK appends /chat/completions itself; storing the request URL
		// would double it and 404.
		Expect(nestedString(body, "model", "endpoint")).To(Equal(remoteBase))
		Expect(credentialExists()).To(BeFalse(), "a public model has no credential")

		Eventually(func() error {
			sec, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, "openclaw-config", metav1.GetOptions{})
			if err != nil {
				return err
			}
			rendered, err := renderedProvider(sec.Data["openclaw.json"], modelName)
			if err != nil {
				return err
			}
			if key, _ := rendered["apiKey"].(string); key != gateway.PublicModelAPIKey {
				return fmt.Errorf("provider apiKey = %v, want %q", rendered["apiKey"], gateway.PublicModelAPIKey)
			}
			return nil
		}).Should(Succeed())
	})

	It("rotates an existing credential and removes the model with it", func() {
		By("adding a keyed model (secrets create)")
		_, code, err := fw.SendJSON(ctx, http.MethodPost, fw.APIBase+"/api/llms", map[string]any{
			"name":     modelName,
			"endpoint": remoteBase,
			"apiKey":   "sk-e2e-original",
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
		Expect(credentialValue()).To(Equal("sk-e2e-original"))

		By("rotating it (secrets update)")
		_, code, err = fw.SendJSON(ctx, http.MethodPut, fw.APIBase+modelPath, map[string]any{
			"endpoint": remoteBase,
			"apiKey":   "sk-e2e-rotated",
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
		Expect(credentialValue()).To(Equal("sk-e2e-rotated"))

		By("removing the model (secrets delete)")
		_, code, err = fw.SendJSON(ctx, http.MethodDelete, fw.APIBase+modelPath, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
		// A credential that outlives its model is a live key nothing references.
		Expect(credentialExists()).To(BeFalse(), "the credential Secret must not be orphaned")
	})
})
