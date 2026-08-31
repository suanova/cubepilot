package k8s

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func int64Ptr(v int64) *int64 { return &v }
func boolPtr(v bool) *bool    { return &v }

// AgentSpec carries the inputs shared by the per-user agent resources. The
// user identity is passed per call (each resource builder takes a user).
type AgentSpec struct {
	Namespace    string
	Image        string
	GatewayToken string
	Port         int32
	// AgentUser is the instance owner (the supervisor's CUBEPILOT_AGENT_USER
	// -- it pulls the resolved config for exactly this user).
	AgentUser string
	// CredentialEnv are env vars injecting model credential Secret keys
	// (secretKeyRef, key "apiKey") into the supervisor container. They let the
	// gateway resolve the $CUBEPILOT_LLM_* env refs rendered in openclaw.json
	// without any literal key landing in the config file or the PVC.
	CredentialEnv []corev1.EnvVar
}

func (s AgentSpec) pvcName(user string) string { return ResourceName("data", user) }
func (s AgentSpec) svcName(user string) string { return ResourceName("agent", user) }
func (s AgentSpec) podName(user string) string { return ResourceName("agent", user) }

// DataPVC returns the per-user PVC that persists sessions/memory (FR-M2-004).
func (s AgentSpec) DataPVC(user string) *corev1.PersistentVolumeClaim {
	return s.DataPVCFor(s.pvcName(user), user, "1Gi")
}

// DataPVCFor builds a per-instance data PVC (design §3.2: one PVC per
// instance, source of truth = instance data directory; default 1Gi).
// Name/labels follow the instance identity.
func (s AgentSpec) DataPVCFor(name, instance string, size string) *corev1.PersistentVolumeClaim {
	if size == "" {
		size = "1Gi"
	}
	labels := map[string]string{AgentLabelApp: "true"}
	if instance != "" {
		labels[AgentLabelUser] = instance
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}
}

// Service returns the ClusterIP service exposing the agent gateway on s.Port.
func (s AgentSpec) Service(user string) *corev1.Service {
	return s.ServiceFor(s.svcName(user), user, s.podName(user))
}

// ServiceFor builds the ClusterIP service exposing an agent instance gateway.
// The selector must match the Pod labels (AgentLabelApp + AgentLabelUser =
// instance) so the service actually routes to the instance Pod.
func (s AgentSpec) ServiceFor(name, instance, _ string) *corev1.Service {
	labels := map[string]string{AgentLabelApp: "true"}
	if instance != "" {
		labels[AgentLabelUser] = instance
	}
	selector := map[string]string{AgentLabelApp: "true"}
	if instance != "" {
		selector[AgentLabelUser] = instance
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name:       "gateway",
				Port:       s.Port,
				TargetPort: intstr.FromInt(int(s.Port)),
			}},
		},
	}
}

// Pod returns the per-user OpenClaw gateway Pod. Config is injected from the
// shared openclaw-config Secret (subPath over the PVC); kubeconfig from the
// shared agent-kubeconfig Secret; mutable state lives in the per-user PVC.
func (s AgentSpec) Pod(user string) *corev1.Pod {
	return s.PodFor(s.podName(user), user, s.pvcName(user), s.svcName(user))
}

// PodFor builds the per-user OpenClaw gateway Pod for an agent instance.
// Config is injected from the shared openclaw-config Secret (subPath over the
// PVC); kubeconfig from the shared agent-kubeconfig Secret; mutable state lives
// in the per-instance data PVC (design §3.4: the platform holds zero
// agent-private data).
func (s AgentSpec) PodFor(name, instance, pvcName, svcName string) *corev1.Pod {
	port := s.Port
	labels := map[string]string{AgentLabelApp: "true"}
	if instance != "" {
		labels[AgentLabelUser] = instance
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: ServiceAccountName,
			// Instance minimum privilege (design §6): non-root, seccomp
			// RuntimeDefault, drop ALL capabilities. The image runs as the
			// `node` user (uid/gid 1000); FSGroup makes the mounted PVC
			// group-writable so it can persist sessions (FR-M2-004).
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup:            int64Ptr(1000),
				RunAsNonRoot:       boolPtr(true),
				SeccompProfile:     &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				SupplementalGroups: []int64{1000},
			},
			InitContainers: []corev1.Container{
				// Seed the workspace from the image's read-only layer into the
				// per-instance PVC (design §3.6: runtime caches live on the
				// instance PVC, not the image). The gateway maintains files in
				// the workspace (e.g. TOOLS.md) -- under readOnlyRootFilesystem
				// the image layer is not writable, so the workspace must be on
				// the PVC.
				{
					Name:    "seed-workspace",
					Image:   s.Image,
					Command: []string{"sh", "-c", "mkdir -p /mnt/data/workspace && cp -a /opt/cubepilot/workspace/. /mnt/data/workspace/ 2>/dev/null || true"},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/mnt/data"},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsNonRoot:             boolPtr(true),
						RunAsUser:                int64Ptr(1000),
						RunAsGroup:               int64Ptr(1000),
						AllowPrivilegeEscalation: boolPtr(false),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
				},
			},
			Containers: []corev1.Container{{
				// The supervisor is pid 1: it pulls the resolved agent config
				// (internal API), renders skills into the PVC workspace, and
				// runs the OpenClaw gateway as a child process. Config changes
				// trigger a graceful gateway restart -- the pod is never
				// deleted, so sessions/PVC/IP survive (final architecture).
				Name:    "supervisor",
				Image:   s.Image,
				Command: []string{"cubepilot-supervisor"},
				// The supervisor needs a writable scratch dir (node caches, temp
				// files) but the rest of the filesystem is read-only. RunAsUser
				// is explicit because the image declares a non-numeric user
				// (node) -- kubelet cannot verify non-root from a name alone.
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             boolPtr(true),
					RunAsUser:                int64Ptr(1000),
					RunAsGroup:               int64Ptr(1000),
					ReadOnlyRootFilesystem:   boolPtr(true),
					AllowPrivilegeEscalation: boolPtr(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				Env: append([]corev1.EnvVar{
					{Name: "HOME", Value: "/home/node"},
					{Name: "OPENCLAW_HOME", Value: "/home/node"},
					{Name: "OPENCLAW_STATE_DIR", Value: "/home/node/.openclaw"},
					// The gateway reads openclaw.json from its default path
					// (~/.openclaw/openclaw.json); the supervisor renders it by pulling
					// the operator's config from the internal API.
					{Name: "OPENCLAW_ALLOW_OLDER_BINARY_DESTRUCTIVE_ACTIONS", Value: "1"},
					// Supervisor wiring: which user's resolved config to pull.
					{Name: "CUBEPILOT_AGENT_USER", Value: s.AgentUser},
					{Name: "CUBEPILOT_WORKSPACE", Value: "/home/node/.openclaw/workspace"},
					{
						Name: "OPENCLAW_GATEWAY_TOKEN",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: ConfigSecretName},
								Key:                  "gatewayToken",
							},
						},
					},
				}, s.CredentialEnv...),
				Ports: []corev1.ContainerPort{{ContainerPort: port}},
				VolumeMounts: []corev1.VolumeMount{
					// The workspace is the default ~/.openclaw/workspace (= PVC
					// root / workspace subdir); no explicit mount or env needed.
					{Name: "data", MountPath: "/home/node/.openclaw"},
					{Name: "kubeconfig", MountPath: "/home/node/.kube/config", SubPath: "config"},
					{Name: "scratch", MountPath: "/tmp"},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(int(port))},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
					},
				},
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: KubeconfigSecretName},
					},
				},
				{
					Name: "scratch",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}
}
