package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

const (
	agentContainer  = "supervisor"
	agentWorkspace  = "/home/node/.openclaw/workspace"
	agentConfigPath = "/home/node/.openclaw/openclaw.json"
	// A poll is 10s; two of them leave room for a slow fetch without hiding a
	// supervisor that never converges.
	convergeTimeout = 45 * time.Second
)

var _ = Describe("Workspace artifact ownership", Label("workspace"), func() {
	It("restores platform content the agent changed and leaves the agent's own skill alone", func() {
		ctx := context.Background()
		user := fw.DefaultUser
		name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)

		var pod string
		Eventually(func() error {
			if err := agentStabilityErr(ctx, user); err != nil {
				return err
			}
			var inst v1alpha1.AgentInstance
			if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Name: name, Namespace: fw.Namespace}, &inst); err != nil {
				return err
			}
			pod = inst.Status.PodName
			return nil
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		exec := func(cmd string) (string, error) {
			return fw.Exec(ctx, pod, agentContainer, "sh", "-c", cmd)
		}

		// The platform config is the artifact every instance has.
		before, err := exec("cat " + agentConfigPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(before).To(ContainSubstring(`"gateway"`))

		// A skill the agent authored itself: unmarked, so the platform must not
		// delete it.
		_, err = exec("mkdir -p " + agentWorkspace + "/skills/e2e-agent-skill && " +
			"printf '# agent skill\\n' > " + agentWorkspace + "/skills/e2e-agent-skill/SKILL.md")
		Expect(err).NotTo(HaveOccurred())

		// The instance has platform skills installed (the builtin template ships
		// them). Require one: if none were installed the tamper step below would
		// silently skip, and the skill half of this test would pass without
		// exercising convergence at all.
		out, err := exec("ls -1d " + agentWorkspace + "/skills/*/ | grep -v e2e-agent-skill | head -1")
		Expect(err).NotTo(HaveOccurred())
		platformSkill := strings.TrimSpace(out)
		Expect(platformSkill).NotTo(BeEmpty(),
			"no platform skill installed: the skill-drift half of this test would pass vacuously")

		// Tamper: rewrite the platform's gateway config, edit that skill, and drop
		// the skill's ownership marker. `ls -1d` on a directory glob yields a
		// trailing slash, hence `platformSkill + "SKILL.md"`.
		_, err = exec("printf '{\"rogue\":true}' > " + agentConfigPath)
		Expect(err).NotTo(HaveOccurred())
		// A platform skill's files land read-only: the tarball is built from the
		// embedded skill sources, Go's embed reports those as 0444, and the extract
		// stamps each entry with its source mode. The pod owns the file and runs
		// with full exec, so making it writable is the route an agent takes before
		// editing -- not a workaround. It is not drift either: skill.TreeHash keys
		// on content, so the chmod alone changes nothing and the edit below does.
		_, err = exec("chmod u+w " + platformSkill + "SKILL.md")
		Expect(err).NotTo(HaveOccurred())
		_, err = exec("printf '\\ntampered\\n' >> " + platformSkill + "SKILL.md")
		Expect(err).NotTo(HaveOccurred())
		_, err = exec("rm -f " + platformSkill + ".cubepilot.json")
		Expect(err).NotTo(HaveOccurred())

		// Within a poll or two the platform content is back, and the agent's skill
		// is untouched. A deleted marker is drift too, so the platform must have
		// re-asserted it.
		Eventually(func() error {
			got, err := exec("cat " + agentConfigPath)
			if err != nil {
				return err
			}
			if !strings.Contains(got, `"gateway"`) {
				return fmt.Errorf("openclaw.json still tampered: %s", got)
			}
			if _, err := exec("test -f " + agentWorkspace + "/skills/e2e-agent-skill/SKILL.md"); err != nil {
				return fmt.Errorf("agent-authored skill removed: %w", err)
			}
			if _, err := exec("test -f " + platformSkill + ".cubepilot.json"); err != nil {
				return fmt.Errorf("ownership marker not re-asserted: %w", err)
			}
			return nil
		}, convergeTimeout, 5*time.Second).Should(Succeed())

		// The platform skill is present and back to the platform's content: `cat`
		// fails on a missing file, so this asserts presence and repair at once.
		out, err = exec("cat " + platformSkill + "SKILL.md")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("tampered"))
	})
})
