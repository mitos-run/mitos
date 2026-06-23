// Package v1 is the stable mitos.run/v1 API. It consolidates the three nouns:
// SandboxPool (inline template), Sandbox (single running sandbox, folding
// the former SandboxClaim and SandboxFork into one kind), and Workspace plus
// WorkspaceRevision. The four v1alpha1 kinds (SandboxTemplate, SandboxPool,
// SandboxClaim, SandboxFork) and the v1alpha2 package are deleted; v1 is the
// sole served and stored version.
package v1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the mitos.run/v1 group version.
	GroupVersion = schema.GroupVersion{Group: "mitos.run", Version: "v1"}

	// SchemeBuilder registers the v1 types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the v1 types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
