package v1alpha1

// SandboxPool is the conversion Hub (the controller storage version) for the
// multi-version SandboxPool CRD (ADR 0007, issue #23). The v1alpha2 SandboxPool
// implements conversion.Convertible against it. Declaring the Hub here is a pure
// Go addition: it adds no field and no kubebuilder marker, so the v1alpha1 CRD
// schema does not change.
//
// The storage-version flip to v1alpha2 is the staged continuation (ADR 0007
// OPEN section), not done in this slice; v1alpha1 stays the Hub and the stored
// version.
func (*SandboxPool) Hub() {}
