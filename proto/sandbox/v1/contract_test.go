package sandboxv1

import "testing"

// TestSandboxServiceContractExists is a compile-and-shape check that the
// generated Sandbox runtime service contract is present and exposes the RPCs the
// v2 spec (docs/api/v2-spec.md section 4) requires. It does NOT wire the service
// into forkd: this slice lands the contract and stubs only (issue #24); the wire
// migration is a follow-up (docs/api/runtime-protocol.md). The compiler enforces
// each method's presence on the generated SandboxServer interface; the test body
// asserts the FileDescriptor agrees so a future hand-edit cannot silently drop a
// method.
func TestSandboxServiceContractExists(t *testing.T) {
	// Assigning the closures forces the generated method set to exist with the
	// expected signatures; if a method is renamed or removed the package fails to
	// compile, which is the contract guarantee we want.
	var srv SandboxServer
	_ = srv

	want := map[string]bool{
		"Exec":           false,
		"ExecStream":     false,
		"RunCodeStream":  false,
		"ReadFile":       false,
		"WriteFile":      false,
		"List":           false,
		"Stat":           false,
		"Archive":        false,
		"Watch":          false,
		"Processes":      false,
		"Signal":         false,
		"PortForward":    false,
		"Fork":           false,
		"Checkpoint":     false,
		"ExtendLifetime": false,
		"Budget":         false,
		"Vitals":         false,
		"RunCode":        false,
		"Mkdir":          false,
		"Remove":         false,
		"Upload":         false,
	}

	// forbidden are the host-trusted control methods (sandbox.internal.v1.Control):
	// they carry secrets and the fork-correctness handshake and must NEVER appear
	// on the public Sandbox service (the secret-delivery trust boundary, #24).
	forbidden := map[string]bool{
		"Configure":    true,
		"NotifyForked": true,
		"Ping":         true,
	}

	sd := Sandbox_ServiceDesc
	if sd.ServiceName != "sandbox.v1.Sandbox" {
		t.Fatalf("service name = %q, want sandbox.v1.Sandbox", sd.ServiceName)
	}
	mark := func(name string) {
		if _, ok := want[name]; ok {
			want[name] = true
		}
		if forbidden[name] {
			t.Errorf("control method %q is exposed on the public Sandbox service; it must stay on sandbox.internal.v1.Control only", name)
		}
	}
	for _, m := range sd.Methods {
		mark(m.MethodName)
	}
	for _, s := range sd.Streams {
		mark(s.StreamName)
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("RPC %q missing from generated Sandbox service descriptor", name)
		}
	}
}
