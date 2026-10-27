// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package kube

// The objects' fields that feos-k8s uses, as the API's JSON names them.

// ObjectMeta is an object's metadata.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
}

// Node is a node.
type Node struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     NodeSpec   `json:"spec"`
}

// NodeSpec is a node's spec: unschedulable is a cordon.
type NodeSpec struct {
	Unschedulable bool `json:"unschedulable,omitempty"`
}

// Pod is a pod.
type Pod struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
	Status     PodStatus  `json:"status,omitempty"`
}

// PodSpec is a pod's spec.
type PodSpec struct {
	NodeName                      string       `json:"nodeName,omitempty"`
	Containers                    []Container  `json:"containers"`
	Tolerations                   []Toleration `json:"tolerations,omitempty"`
	PriorityClassName             string       `json:"priorityClassName,omitempty"`
	TerminationGracePeriodSeconds *int64       `json:"terminationGracePeriodSeconds,omitempty"`
	AutomountServiceAccountToken  *bool        `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks            *bool        `json:"enableServiceLinks,omitempty"`
}

// Container is a container of a pod.
type Container struct {
	Name            string               `json:"name"`
	Image           string               `json:"image"`
	Resources       ResourceRequirements `json:"resources,omitempty"`
	SecurityContext *SecurityContext     `json:"securityContext,omitempty"`
}

// ResourceRequirements are a container's requests and limits, as
// quantities ("1m", "8Mi").
type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// SecurityContext is a container's security context.
type SecurityContext struct {
	RunAsNonRoot             *bool         `json:"runAsNonRoot,omitempty"`
	RunAsUser                *int64        `json:"runAsUser,omitempty"`
	AllowPrivilegeEscalation *bool         `json:"allowPrivilegeEscalation,omitempty"`
	ReadOnlyRootFilesystem   *bool         `json:"readOnlyRootFilesystem,omitempty"`
	Capabilities             *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities are a container's added and dropped capabilities.
type Capabilities struct {
	Drop []string `json:"drop,omitempty"`
}

// Toleration is a pod's toleration of a taint; Operator "Exists" without a
// key tolerates every taint.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// PodStatus is a pod's status.
type PodStatus struct {
	Phase string `json:"phase,omitempty"`
}

// The pod phases that end a pod.
const (
	PodSucceeded = "Succeeded"
	PodFailed    = "Failed"
)

// Secret is a secret; Data's values travel as base64, which encoding/json
// does for []byte.
type Secret struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}
