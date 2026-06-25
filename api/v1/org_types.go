package v1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Org is the cluster-scoped account record for one hosted-SaaS tenant (issue
// #288). It is the declarative source the org-namespace reconciler provisions a
// per-org isolation namespace from: mitos-org-<id>, where <id> is the Org's
// name. Each org's sandboxes, pools, warm husk pods, claims, and secrets live in
// that namespace under a default-deny NetworkPolicy and a ResourceQuota ceiling.
//
// The Org is CLUSTER-SCOPED because it OWNS a cluster-scoped Namespace: an owner
// reference from a namespaced object to a cluster-scoped object is invalid, so
// the owner (Org) must itself be cluster-scoped for the namespace to be
// garbage-collected when the org is deleted.
//
// Self-host is unaffected: the OrgReconciler only runs when the controller is
// started with --enable-org-tenancy (default false), so a single-tenant install
// never reconciles Orgs.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=org
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Org struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired org account configuration: its display name, tier, and
	// an optional quota override.
	Spec OrgSpec `json:"spec"`

	// Status reports the provisioned namespace, phase, and conditions.
	Status OrgStatus `json:"status,omitempty"`
}

// OrgSpec is the desired state of an Org account.
type OrgSpec struct {
	// DisplayName is the human-facing name of the org (the company or account
	// name). It is presentational only; the org id is the Org's metadata.name and
	// is what NamespaceForOrg maps to a namespace.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Tier is the org's plan tier (for example "free", "pro", "enterprise"). It is
	// a label the control plane and pricing layer interpret; the reconciler does
	// not branch on it today (quota overrides are explicit via Quota). Empty by
	// default.
	// +optional
	Tier string `json:"tier,omitempty"`

	// Quota optionally overrides the controller-default per-org ceilings. When
	// set, its fields populate the org namespace's ResourceQuota; unset fields fall
	// back to the controller defaults (the --org-default-* flags). A nil Quota uses
	// the controller defaults entirely. This is the per-org abuse-control surface.
	// +optional
	Quota *OrgQuota `json:"quota,omitempty"`
}

// OrgQuota overrides the controller-default per-org resource ceilings. A zero or
// unset field falls back to the controller default rather than meaning "zero",
// so a partial override (only MaxSandboxes, say) keeps the defaults for the rest.
type OrgQuota struct {
	// MaxSandboxes caps the number of Sandbox-bearing pods (the count/pods ceiling)
	// in the org namespace. Zero falls back to the controller default. This is the
	// primary abuse-control primitive: it bounds how much an org can schedule.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxSandboxes int32 `json:"maxSandboxes,omitempty"`

	// MaxPods caps the total pod count in the org namespace (husk pods plus any
	// supporting pods). Zero falls back to the controller default. It is a separate
	// ceiling from MaxSandboxes so warm husk capacity and active sandboxes can be
	// bounded independently.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxPods int32 `json:"maxPods,omitempty"`

	// CPU caps the aggregate CPU limit across the org namespace. The zero quantity
	// falls back to the controller default.
	// +optional
	CPU resource.Quantity `json:"cpu,omitempty"`

	// Memory caps the aggregate memory limit across the org namespace. The zero
	// quantity falls back to the controller default.
	// +optional
	Memory resource.Quantity `json:"memory,omitempty"`
}

// OrgPhase is the lifecycle phase of an Org's namespace provisioning.
type OrgPhase string

const (
	// OrgProvisioning is set while the reconciler is creating or updating the org
	// namespace stack.
	OrgProvisioning OrgPhase = "Provisioning"
	// OrgReady is set once the namespace and its full isolation stack are present.
	OrgReady OrgPhase = "Ready"
	// OrgFailed is set when provisioning a stack object errored; the reconcile
	// requeues.
	OrgFailed OrgPhase = "Failed"
)

// OrgStatus is the observed state of an Org.
type OrgStatus struct {
	// Namespace is the org's provisioned isolation namespace (mitos-org-<id>).
	// Empty until the first successful reconcile.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Phase is Provisioning, Ready, or Failed.
	// +optional
	Phase OrgPhase `json:"phase,omitempty"`

	// Conditions carries the standard condition set (Ready) for the org's
	// namespace provisioning.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the Org generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// OrgList is the list type for Org.
//
// +kubebuilder:object:root=true
type OrgList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Org `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Org{}, &OrgList{})
}
