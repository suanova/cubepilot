package k8s

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func int64Ptr(v int64) *int64 { return &v }

// AgentSpec carries the inputs shared by the per-user agent resources. The
// user identity is passed per call (each resource builder takes a user).
// ReadOnly switches to the inspection identity + read-only kubeconfig
// (design §5.4: 巡检实例挂载专用只读 kubeconfig, 即使被注入也无法写入).
type AgentSpec struct {
	Namespace    string
	Image        string
	GatewayToken string
	Port         int32
	ReadOnly     bool
}

func (s AgentSpec) saName() string {
	if s.ReadOnly {
		return ReadOnlyServiceAccountName
	}
	return ServiceAccountName
}

func (s AgentSpec) kubeconfigSecret() string {
	if s.ReadOnly {
		return ReadOnlyKubeconfigSecretName
	}
	return KubeconfigSecretName
}

func (s AgentSpec) podPrefix() string {
	if s.ReadOnly {
		return "inspect"
	}
	return "agent"
}

func (s AgentSpec) pvcName(user string) string {
	if s.ReadOnly {
		return ResourceName("inspect-data", user)
	}
	return ResourceName("data", user)
}
func (s AgentSpec) svcName(user string) string {
	if s.ReadOnly {
		return ResourceName("inspect", user)
	}
	return ResourceName("agent", user)
}
func (s AgentSpec) podName(user string) string {
	if s.ReadOnly {
		return ResourceName("inspect", user)
	}
	return ResourceName("agent", user)
}

func agentLabels(user string) map[string]string {
	return map[string]string{AgentLabelApp: "true", AgentLabelUser: user}
}

// inspectLabels marks read-only inspection resources. They intentionally do
// NOT carry AgentLabelApp so the Instance Manager's reconcile (selector
// cubepilot-agent=true) never mistakes an inspection Pod for the user's
// conversation instance — and can never rebuild an inspection Pod with the
// privileged spec (design §5.4 只读边界不被自愈逻辑破坏).
func inspectLabels(user string) map[string]string {
	return map[string]string{AgentLabelInspect: "true", AgentLabelUser: user}
}

// labels returns the resource labels for this spec variant.
func (s AgentSpec) labels(user string) map[string]string {
	if s.ReadOnly {
		return inspectLabels(user)
	}
	return agentLabels(user)
}

// DataPVC returns the per-user PVC that persists sessions/memory (FR-M2-004).
func (s AgentSpec) DataPVC(user string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.pvcName(user),
			Namespace: s.Namespace,
			Labels:    s.labels(user),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

// Service returns the ClusterIP service exposing the agent gateway on s.Port.
func (s AgentSpec) Service(user string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.svcName(user),
			Namespace: s.Namespace,
			Labels:    s.labels(user),
		},
		Spec: corev1.ServiceSpec{
			Selector: s.labels(user),
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
	port := s.Port
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.podName(user),
			Namespace: s.Namespace,
			Labels:    s.labels(user),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: s.saName(),
			// The image runs as the `node` user (uid/gid 1000); make the mounted
			// PVC group-writable so it can persist sessions (FR-M2-004).
			SecurityContext: &corev1.PodSecurityContext{FSGroup: int64Ptr(1000)},
			Containers: []corev1.Container{{
				Name:    "gateway",
				Image:   s.Image,
				Command: []string{"node", "dist/index.js", "gateway", "--bind", "lan", "--port", "18789"},
				Env: []corev1.EnvVar{
					{Name: "HOME", Value: "/home/node"},
					{Name: "OPENCLAW_HOME", Value: "/home/node"},
					{Name: "OPENCLAW_STATE_DIR", Value: "/home/node/.openclaw"},
					{Name: "OPENCLAW_CONFIG_PATH", Value: "/home/node/.openclaw/openclaw.json"},
					{Name: "OPENCLAW_WORKSPACE_DIR", Value: "/opt/cubepilot/workspace"},
					{Name: "OPENCLAW_ALLOW_OLDER_BINARY_DESTRUCTIVE_ACTIONS", Value: "1"},
					{
						Name: "OPENCLAW_GATEWAY_TOKEN",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: ConfigSecretName},
								Key:                  "gatewayToken",
							},
						},
					},
				},
				Ports: []corev1.ContainerPort{{ContainerPort: port}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/home/node/.openclaw"},
					{Name: "config", MountPath: "/home/node/.openclaw/openclaw.json", SubPath: "openclaw.json"},
					{Name: "kubeconfig", MountPath: "/home/node/.kube/config", SubPath: "config"},
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
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: s.pvcName(user)},
					},
				},
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: ConfigSecretName},
					},
				},
				{
					Name: "kubeconfig",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: s.kubeconfigSecret()},
					},
				},
			},
		},
	}
}
