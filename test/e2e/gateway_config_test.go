package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/controller"
	"github.com/suanova/cubepilot/internal/k8s"
)

var _ = Describe("Gateway config", func() {
	ctx := context.Background()

	It("renders the shared openclaw-config secret", func() {
		// Derive the expected primary from the live template so model overrides
		// in CI stay robust.
		tpl := &v1alpha1.AgentTemplate{}
		Eventually(func() error {
			return fw.CtrlClient.Get(ctx, types.NamespacedName{Name: controller.BuiltinAgentName}, tpl)
		}).Should(Succeed())
		expectedPrimary := tpl.Spec.DefaultModel + "/" + tpl.Spec.DefaultModel

		// The operator creates the Secret with the gatewayToken first and writes
		// openclaw.json on a later reconcile, so poll until both are present.
		var sec *corev1.Secret
		Eventually(func() error {
			var err error
			sec, err = fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, k8s.ConfigSecretName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if len(sec.Data["gatewayToken"]) == 0 {
				return fmt.Errorf("gatewayToken empty")
			}
			if len(sec.Data["openclaw.json"]) == 0 {
				return fmt.Errorf("openclaw.json missing")
			}
			return nil
		}).Should(Succeed())

		Expect(string(sec.Data["gatewayToken"])).NotTo(BeEmpty())

		var cfg map[string]any
		Expect(json.Unmarshal(sec.Data["openclaw.json"], &cfg)).To(Succeed())
		gw, ok := cfg["gateway"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(gw["mode"]).To(Equal("local"))
		Expect(gw["port"]).To(BeNumerically("==", 18789))

		defaults, ok := cfg["agents"].(map[string]any)["defaults"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(defaults["model"].(map[string]any)["primary"]).To(Equal(expectedPrimary))

		// The apiKey is rendered as a file SecretRef into the supervisor-written
		// keys.json -- never a literal in the config.
		providers, ok := cfg["models"].(map[string]any)["providers"].(map[string]any)
		Expect(ok).To(BeTrue())
		prov, ok := providers[tpl.Spec.DefaultModel].(map[string]any)
		Expect(ok).To(BeTrue(), "provider %s should be rendered", tpl.Spec.DefaultModel)
		apiKey, ok := prov["apiKey"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(apiKey["source"]).To(Equal("file"))
		Expect(apiKey["provider"]).To(Equal(k8s.CredProviderName))
	})

	It("serves the internal per-user config endpoints", func() {
		user := fw.Users[0]

		By("GET /internal/gateway/config/{user}")
		Eventually(func() error {
			cfg, code, err := fw.GetJSON(ctx, fw.APIBase+"/internal/gateway/config/"+user, nil)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("gateway config returned %d", code)
			}
			gw, ok := cfg["gateway"].(map[string]any)
			if !ok || gw["mode"] != "local" {
				return fmt.Errorf("unexpected gateway config: %v", cfg["gateway"])
			}
			return nil
		}).Should(Succeed())

		By("GET /internal/agents/{user}/config")
		Eventually(func() error {
			cfg, code, err := fw.GetJSON(ctx, fw.APIBase+"/internal/agents/"+user+"/config", nil)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("agent config returned %d", code)
			}
			rev, _ := cfg["revision"].(string)
			if rev == "" {
				return fmt.Errorf("agent config revision empty")
			}
			return nil
		}).Should(Succeed())
	})
})
