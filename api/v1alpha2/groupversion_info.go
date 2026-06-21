// Package v1alpha2 is the consolidated three-noun API for mitos.run (issue #23,
// ADR 0007). It collapses the four v1alpha1 kinds (SandboxTemplate, SandboxPool,
// SandboxClaim, SandboxFork) into three nouns: SandboxPool (inline template +
// optional templateRef), Sandbox (one running sandbox; source oneof
// {poolRef, fromSandbox, fromRevision} + replicas, folding claim and fork into
// one kind), and Workspace (unchanged, still served from v1alpha1).
//
// This package is ADDITIVE: it is served alongside v1alpha1 during the
// migration. The breaking storage-version flip, the removal of SandboxTemplate
// and SandboxFork, and the controller/SDK/facade cutover are the staged
// continuation (ADR 0007 "OPEN" section), not done here.
package v1alpha2

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the mitos.run/v1alpha2 group version. It shares the
	// mitos.run group with v1alpha1, so SandboxPool is served as a second
	// version of the SAME CRD with a conversion webhook between them.
	GroupVersion = schema.GroupVersion{Group: "mitos.run", Version: "v1alpha2"}

	// SchemeBuilder registers the v1alpha2 types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the v1alpha2 types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
