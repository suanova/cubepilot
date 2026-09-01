package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suanova/cubepilot/internal/controller"
)

// nestedString walks a decoded JSON object through the given keys, returning ""
// when any step is missing or not an object/string.
func nestedString(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

var _ = Describe("Public API", func() {
	ctx := context.Background()

	It("serves /healthz", func() {
		_, code, err := fw.GetRaw(ctx, fw.APIBase+"/healthz")
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
	})

	It("lists agent templates including the builtin", func() {
		// Ginkgo randomizes spec order, so this spec may run before the
		// operator's bootstrap has created the template (and the API's client
		// cache may lag) -- poll until the builtin appears.
		var names []string
		Eventually(func() error {
			data, code, err := fw.GetJSON(ctx, fw.APIBase+"/api/agenttemplates", nil)
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("agenttemplates returned %d", code)
			}
			items, ok := data["agentTemplates"].([]any)
			if !ok {
				return fmt.Errorf("agentTemplates key missing: %v", data)
			}
			names = names[:0]
			for _, it := range items {
				names = append(names, nestedString(it.(map[string]any), "metadata", "name"))
			}
			for _, n := range names {
				if n == controller.BuiltinAgentName {
					return nil
				}
			}
			return fmt.Errorf("builtin template %s not listed yet", controller.BuiltinAgentName)
		}).Should(Succeed())
		Expect(names).To(ContainElement(controller.BuiltinAgentName))
	})

	It("returns the caller's own instances", func() {
		// The operator instantiates the builtin agent per user after its
		// bootstrap (which publishes skills first), so wait until the caller's
		// own instance appears rather than asserting on the first 200.
		Eventually(func() error {
			data, code, err := fw.GetJSON(ctx, fw.APIBase+"/api/instances",
				map[string]string{"X-CubePilot-User": fw.DefaultUser})
			if err != nil {
				return err
			}
			if code != http.StatusOK {
				return fmt.Errorf("instances returned %d", code)
			}
			items, ok := data["instances"].([]any)
			if !ok {
				return fmt.Errorf("instances key missing: %v", data)
			}
			for _, it := range items {
				if nestedString(it.(map[string]any), "metadata", "name") == controller.InstanceNameFor(fw.DefaultUser, controller.BuiltinAgentName) {
					return nil
				}
			}
			return fmt.Errorf("own instance %q not present in %v", controller.InstanceNameFor(fw.DefaultUser, controller.BuiltinAgentName), items)
		}).Should(Succeed())
	})

	It("serves the portal HTML", func() {
		body, code, err := fw.GetRaw(ctx, fw.PortalBase+"/")
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK))
		Expect(strings.ToLower(string(body))).To(ContainSubstring("<html"))
	})
})
