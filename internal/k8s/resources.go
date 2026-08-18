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
type AgentSpec struct {
	Namespace    string
	Image        string
	GatewayToken string
	Port         int32
}

func (s AgentSpec) pvcName(user string) string { return ResourceName("data", user) }
func (s AgentSpec) svcName(user string) string { return ResourceName("agent", user) }
func (s AgentSpec) podName(user string) string { return ResourceName("agent", user) }

func agentLabels(user string) map[string]string {
	return map[string]string{AgentLabelApp: "true", AgentLabelUser: user}
}

func (s AgentSpec) labels(user string) map[string]string {
	return agentLabels(user)
}

// DataPVC returns the per-user PVC that persists sessions/memory (FR-M2-004).
func (s AgentSpec) DataPVC(user string) *corev1.PersistentVolumeClaim {
	return s.DataPVCFor(s.pvcName(user), user, "1Gi")
}

// DataPVCFor builds a per-instance data PVC (设计 §3.2: 每实例独立 PVC,
// 真源 = 实例数据目录; 默认 1Gi). Name/labels follow the instance identity.
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
// in the per-instance data PVC (设计 §3.4: 平台零持有 agent 私有数据).
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
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
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
						Secret: &corev1.SecretVolumeSource{SecretName: KubeconfigSecretName},
					},
				},
			},
		},
	}
}
