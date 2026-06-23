package v1

import "fmt"

// ValidateCreate enforces that exactly one of spec.template or spec.templateRef
// is set (the Deployment-embeds-PodSpec pattern, ADR 0007).
func (p *SandboxPool) ValidateCreate() error {
	hasInline := p.Spec.Template != nil
	hasRef := p.Spec.TemplateRef != nil
	if hasInline == hasRef {
		return fmt.Errorf("spec must set exactly one of template or templateRef")
	}
	return nil
}

// ValidateCreate enforces that exactly one of source.poolRef, source.fromSandbox,
// or source.fromRevision is set.
func (s *Sandbox) ValidateCreate() error {
	n := 0
	if s.Spec.Source.PoolRef != nil {
		n++
	}
	if s.Spec.Source.FromSandbox != nil {
		n++
	}
	if s.Spec.Source.FromRevision != nil {
		n++
	}
	if n != 1 {
		return fmt.Errorf("spec.source must set exactly one of poolRef, fromSandbox, or fromRevision")
	}
	return nil
}
