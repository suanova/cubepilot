// Package e2e contains the CubePilot end-to-end test suite. It runs against a
// deployed stack (kind cluster + helm, brought up by scripts/setup.sh) and
// asserts the operator's bootstrap/provisioning behavior, the rendered gateway
// config, the public REST API and (optionally) the SSE chat flow. The suite
// structure mirrors the OCM project's test/e2e pattern: a single suite with a
// BeforeSuite that builds clients and waits for readiness, and one feature
// file per concern.
package e2e

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/suanova/cubepilot/test/e2e/framework"
)

var e2eKubeconfig string
var fw *framework.Framework

func init() {
	// OCM pattern: register flags in init(), fall back to the KUBECONFIG env.
	flag.StringVar(&e2eKubeconfig, "e2e-kubeconfig", "",
		"path to the kubeconfig for the deployed cluster (default: $KUBECONFIG)")
}

func TestE2E(t *testing.T) {
	if e2eKubeconfig == "" {
		e2eKubeconfig = os.Getenv("KUBECONFIG")
	}
	// A bare `go test ./test/e2e` (no kubeconfig) should skip cleanly instead of
	// failing; the Makefile/CI package scoping is the real gate.
	if e2eKubeconfig == "" {
		t.Skip("no kubeconfig: set -e2e-kubeconfig or KUBECONFIG (this is not an e2e run)")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "cubepilot e2e suite")
}

var _ = BeforeSuite(func() {
	path, err := filepath.Abs(e2eKubeconfig)
	Expect(err).NotTo(HaveOccurred())

	// Suite-wide Eventually defaults (OCM pattern).
	SetDefaultEventuallyTimeout(180 * time.Second)
	SetDefaultEventuallyPollingInterval(5 * time.Second)

	By("building kubernetes clients")
	fw, err = framework.New(path)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for the operator / api / web deployments to be ready")
	ctx := context.Background()
	Eventually(func() error {
		for _, name := range []string{"cubepilot-operator", "cubepilot-api", "cubepilot-web"} {
			if err := fw.CheckDeploymentReady(ctx, name); err != nil {
				return err
			}
		}
		return nil
	}).Should(Succeed())

	By("opening port-forwards to the api and portal services")
	apiURL, stopAPI, err := fw.PortForward(ctx, "cubepilot-api", 8080)
	Expect(err).NotTo(HaveOccurred())
	fw.APIBase = apiURL
	portalURL, stopPortal, err := fw.PortForward(ctx, "cubepilot", 8080)
	Expect(err).NotTo(HaveOccurred())
	fw.PortalBase = portalURL
	DeferCleanup(func() { stopAPI(); stopPortal() })
})
