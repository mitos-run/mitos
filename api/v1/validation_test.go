package v1

import "testing"

func TestSandboxPoolRequiresExactlyOneTemplate(t *testing.T) {
	p := &SandboxPool{Spec: SandboxPoolSpec{}}
	if err := p.ValidateCreate(); err == nil {
		t.Fatal("expected error when neither template nor templateRef is set")
	}
	p.Spec.Template = &PoolTemplateSpec{Image: "x"}
	p.Spec.TemplateRef = &LocalObjectReference{Name: "y"}
	if err := p.ValidateCreate(); err == nil {
		t.Fatal("expected error when both template and templateRef are set")
	}
	p.Spec.TemplateRef = nil
	if err := p.ValidateCreate(); err != nil {
		t.Fatalf("inline template alone should be valid: %v", err)
	}
}

func TestSandboxRequiresExactlyOneSource(t *testing.T) {
	s := &Sandbox{Spec: SandboxSpec{}}
	if err := s.ValidateCreate(); err == nil {
		t.Fatal("expected error when no source is set")
	}
	s.Spec.Source.PoolRef = &LocalObjectReference{Name: "p"}
	s.Spec.Source.FromSandbox = &FromSandboxSource{Name: "q"}
	if err := s.ValidateCreate(); err == nil {
		t.Fatal("expected error when two sources are set")
	}
}
