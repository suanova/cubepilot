package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func testAgentSpec() AgentSpec {
	return AgentSpec{
		Namespace:    "cubepilot",
		Image:        "registry.example/cubepilot-agent:test",
		GatewayToken: "test-token",
		Port:         8080,
		AgentUser:    "alice",
	}
}

// containerByName returns the named container from the rendered pod.
func containerByName(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.InitContainers {
		if c.Name == name {
			return c
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("container %q not found in pod %q", name, pod.Name)
	return corev1.Container{}
}

// assertNonPrivilegedContainer checks the per-container baseline fields every
// runtime container must carry (design §6: non-root, no privilege escalation,
// drop ALL capabilities, read-only root filesystem).
func assertNonPrivilegedContainer(t *testing.T, c corev1.Container) {
	t.Helper()
	if c.SecurityContext == nil {
		t.Fatalf("container %q: SecurityContext is nil", c.Name)
	}
	sc := c.SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("container %q: RunAsNonRoot not set to true", c.Name)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("container %q: RunAsUser = %v, want 1000", c.Name, sc.RunAsUser)
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup != 1000 {
		t.Errorf("container %q: RunAsGroup = %v, want 1000", c.Name, sc.RunAsGroup)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("container %q: AllowPrivilegeEscalation not set to false", c.Name)
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("container %q: ReadOnlyRootFilesystem not set to true", c.Name)
	}
	if sc.Capabilities == nil {
		t.Fatalf("container %q: Capabilities is nil", c.Name)
	}
	dropAll := false
	for _, cap := range sc.Capabilities.Drop {
		if cap == "ALL" {
			dropAll = true
			break
		}
	}
	if !dropAll {
		t.Errorf("container %q: Capabilities.Drop does not include ALL", c.Name)
	}
}

// TestPodForSecurityBaseline verifies every runtime container of a rendered
// instance Pod carries the design §6 minimum-privilege baseline: non-root,
// seccomp RuntimeDefault, drop ALL capabilities, no privilege escalation, and
// a read-only root filesystem.
func TestPodForSecurityBaseline(t *testing.T) {
	pod := testAgentSpec().PodFor("agent-alice", "alice", "data-alice", "agent-alice")

	psc := pod.Spec.SecurityContext
	if psc == nil {
		t.Fatal("pod SecurityContext is nil")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("pod RunAsNonRoot not set to true")
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod SeccompProfile = %+v, want RuntimeDefault", psc.SeccompProfile)
	}

	// seed-workspace is an InitContainer (writes only to the mounted PVC).
	assertNonPrivilegedContainer(t, containerByName(t, pod, "seed-workspace"))
	assertNonPrivilegedContainer(t, containerByName(t, pod, "supervisor"))
}

// TestPodForResourceLimits verifies the supervisor container declares resource
// requests and limits so the pod is scheduleable and bounded.
func TestPodForResourceLimits(t *testing.T) {
	pod := testAgentSpec().PodFor("agent-alice", "alice", "data-alice", "agent-alice")

	want := []struct {
		name     string
		requests map[corev1.ResourceName]string
		limits   map[corev1.ResourceName]string
	}{
		{
			name:     "supervisor",
			requests: map[corev1.ResourceName]string{corev1.ResourceCPU: "100m", corev1.ResourceMemory: "512Mi"},
			limits:   map[corev1.ResourceName]string{corev1.ResourceCPU: "1", corev1.ResourceMemory: "2Gi"},
		},
	}

	for _, tc := range want {
		c := containerByName(t, pod, tc.name)
		for name, wantStr := range tc.requests {
			got, ok := c.Resources.Requests[name]
			if !ok || got.Cmp(resource.MustParse(wantStr)) != 0 {
				t.Errorf("container %q: Requests[%s] = %v, want %s", tc.name, name, got.String(), wantStr)
			}
		}
		for name, wantStr := range tc.limits {
			got, ok := c.Resources.Limits[name]
			if !ok || got.Cmp(resource.MustParse(wantStr)) != 0 {
				t.Errorf("container %q: Limits[%s] = %v, want %s", tc.name, name, got.String(), wantStr)
			}
		}
	}
}

// TestPodForGatewayCache verifies the supervisor container gets a writable
// emptyDir at /home/node/.cache: the OpenClaw gateway (>= 2026.8.1) keeps its
// SQLite worker cache under ~/.cache and hard-fails to start when the dir is
// read-only, which it is by default under the container's readOnlyRootFilesystem.
func TestPodForGatewayCache(t *testing.T) {
	pod := testAgentSpec().PodFor("agent-alice", "alice", "data-alice", "agent-alice")

	sc := containerByName(t, pod, "supervisor")
	var cacheMount *corev1.VolumeMount
	for i := range sc.VolumeMounts {
		if sc.VolumeMounts[i].MountPath == "/home/node/.cache" {
			cacheMount = &sc.VolumeMounts[i]
			break
		}
	}
	if cacheMount == nil {
		t.Fatal("supervisor: no mount at /home/node/.cache (gateway cache dir)")
	}

	var cacheVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == cacheMount.Name {
			cacheVol = &pod.Spec.Volumes[i]
			break
		}
	}
	if cacheVol == nil {
		t.Fatalf("supervisor: mount %q has no matching volume", cacheMount.Name)
	}
	if cacheVol.EmptyDir == nil {
		t.Errorf("volume %q is not an emptyDir", cacheMount.Name)
	}
}

// TestPodForDualKubeconfig verifies the issue #19 dual-kubeconfig layout: the
// default ~/.kube/config mounts the per-user kubeconfig Secret (falling back to
// the shared agent-kubeconfig when unset), the platform (agent-kubeconfig) is
// mounted on a secondary discovery path, and both paths are exposed as env so
// the schema-discovery skill can pass --kubeconfig.
func TestPodForDualKubeconfig(t *testing.T) {
	t.Run("per-user secret present", func(t *testing.T) {
		spec := testAgentSpec()
		spec.UserKubeconfigSecret = "alice-kubeconfig"
		pod := spec.PodFor("agent-alice", "alice", "data-alice", "agent-alice")

		assertKubeconfigVol(t, pod, "kubeconfig", "alice-kubeconfig", UserKubeconfigPath)
		assertKubeconfigVol(t, pod, "platform-kubeconfig", KubeconfigSecretName, PlatformKubeconfigPath)
		assertKubeconfigEnv(t, pod, UserKubeconfigEnv, UserKubeconfigPath)
		assertKubeconfigEnv(t, pod, PlatformKubeconfigEnv, PlatformKubeconfigPath)
	})

	t.Run("no per-user secret -> SA fallback", func(t *testing.T) {
		pod := testAgentSpec().PodFor("agent-alice", "alice", "data-alice", "agent-alice")
		assertKubeconfigVol(t, pod, "kubeconfig", KubeconfigSecretName, UserKubeconfigPath)
	})
}

// assertKubeconfigVol finds a Secret volume by name, checks its SecretName and
// that the supervisor mounts it (SubPath config) at wantPath.
func assertKubeconfigVol(t *testing.T, pod *corev1.Pod, volName, wantSecret, wantPath string) {
	t.Helper()
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == volName {
			vol = &pod.Spec.Volumes[i]
			break
		}
	}
	if vol == nil {
		t.Fatalf("pod has no volume %q", volName)
	}
	if vol.Secret == nil || vol.Secret.SecretName != wantSecret {
		t.Errorf("volume %q secret = %+v, want %q", volName, vol.Secret, wantSecret)
	}
	found := false
	for _, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == volName && m.MountPath == wantPath && m.SubPath == "config" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("volume %q not mounted at %s (subPath config)", volName, wantPath)
	}
}

// assertKubeconfigEnv checks the supervisor carries a kubeconfig-path env var.
func assertKubeconfigEnv(t *testing.T, pod *corev1.Pod, envName, want string) {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == envName {
				if e.Value != want {
					t.Errorf("env %s = %q, want %q", envName, e.Value, want)
				}
				return
			}
		}
	}
	t.Errorf("container supervisor has no env %s", envName)
}
