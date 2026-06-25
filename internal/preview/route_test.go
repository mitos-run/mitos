package preview

import "testing"

func TestParseHost(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		domain string
		wantID string
		wantOK bool
	}{
		{"plain", "sb-abc.example.com", "example.com", "sb-abc", true},
		{"with port", "sb-abc.example.com:8443", "example.com", "sb-abc", true},
		{"uppercase host normalized", "SB-ABC.Example.com", "example.com", "sb-abc", true},
		{"wrong suffix", "sb-abc.other.com", "example.com", "", false},
		{"extra labels rejected", "a.b.example.com", "example.com", "", false},
		{"empty sandbox id", ".example.com", "example.com", "", false},
		{"bare domain", "example.com", "example.com", "", false},
		{"extra labels in id rejected", "a.b.c.example.com", "example.com", "", false},
		{"empty host", "", "example.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := ParseHost(tc.host, tc.domain)
			if ok != tc.wantOK {
				t.Fatalf("ParseHost(%q,%q) ok=%v want %v", tc.host, tc.domain, ok, tc.wantOK)
			}
			if id != tc.wantID {
				t.Errorf("ParseHost(%q,%q) id=%q want %q", tc.host, tc.domain, id, tc.wantID)
			}
		})
	}
}

func TestRouteTableLookupMiss(t *testing.T) {
	rt := NewRouteTable()
	if _, ok := rt.Lookup("nope"); ok {
		t.Fatal("expected miss on empty table")
	}
}

func TestRouteTableAddOnReadyRemoveOnTerminate(t *testing.T) {
	rt := NewRouteTable()
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.5:9091", Token: "tok-1"})
	r, ok := rt.Lookup("sb-1")
	if !ok {
		t.Fatal("expected sb-1 present after Upsert")
	}
	if r.Backend != "10.0.0.5:9091" || r.Token != "tok-1" {
		t.Errorf("got %+v", r)
	}
	rt.Remove("sb-1")
	if _, ok := rt.Lookup("sb-1"); ok {
		t.Fatal("expected sb-1 gone after Remove")
	}
}

func TestRouteTableUpsertUpdates(t *testing.T) {
	rt := NewRouteTable()
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.5:9091", Token: "a"})
	rt.Upsert(Route{SandboxID: "sb-1", Backend: "10.0.0.9:9091", Token: "b"})
	r, _ := rt.Lookup("sb-1")
	if r.Backend != "10.0.0.9:9091" || r.Token != "b" {
		t.Errorf("expected upsert to replace, got %+v", r)
	}
	if rt.Len() != 1 {
		t.Errorf("Len = %d, want 1", rt.Len())
	}
}

// fakeSource is an injectable claim source for Sync testing.
type fakeSource struct {
	claims []ClaimState
}

func (f fakeSource) ReadyRoutes() []ClaimState { return f.claims }

func TestParseHostSingleLabel(t *testing.T) {
	cases := []struct {
		host, domain, want string
		ok                 bool
	}{
		{"openclaw.mitos.run", "mitos.run", "openclaw", true},
		{"8000-sbx1.mitos.run", "mitos.run", "8000-sbx1", true},
		{"OpenClaw.Mitos.Run", "mitos.run", "openclaw", true},       // case-insensitive
		{"openclaw.mitos.run:443", "mitos.run", "openclaw", true},   // strips port
		{"a.b.mitos.run", "mitos.run", "", false},                   // two labels: reject
		{"mitos.run", "mitos.run", "", false},                       // apex: no label
		{"openclaw.evil.com", "mitos.run", "", false},               // wrong domain
		{"", "mitos.run", "", false},
		{"openclaw.sandbox.example.com", "sandbox.example.com", "openclaw", true}, // configurable domain
	}
	for _, c := range cases {
		got, ok := ParseHost(c.host, c.domain)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseHost(%q,%q)=(%q,%v) want (%q,%v)", c.host, c.domain, got, ok, c.want, c.ok)
		}
	}
}

func TestIsReservedLabel(t *testing.T) {
	for _, r := range []string{"www", "app", "api", "console", "admin", "auth", "login"} {
		if !IsReservedLabel(r) {
			t.Errorf("expected %q reserved", r)
		}
	}
	for _, ok := range []string{"openclaw", "8000-sbx1", "myapp"} {
		if IsReservedLabel(ok) {
			t.Errorf("did not expect %q reserved", ok)
		}
	}
}

func TestSyncAddsReadyRemovesTerminated(t *testing.T) {
	rt := NewRouteTable()

	// First sync: two Ready claims become routes.
	src := &fakeSource{claims: []ClaimState{
		{SandboxID: "sb-1", Backend: "10.0.0.1:9091", Token: "t1", Ready: true},
		{SandboxID: "sb-2", Backend: "10.0.0.2:9091", Token: "t2", Ready: true},
		{SandboxID: "sb-3", Backend: "", Token: "t3", Ready: false}, // not ready: skipped
	}}
	rt.Sync(src.ReadyRoutes())
	if rt.Len() != 2 {
		t.Fatalf("after first sync Len=%d want 2", rt.Len())
	}
	if _, ok := rt.Lookup("sb-3"); ok {
		t.Error("not-ready claim must not be routed")
	}

	// Second sync: sb-1 terminated (dropped from source), sb-2 stays, sb-4 new.
	src.claims = []ClaimState{
		{SandboxID: "sb-2", Backend: "10.0.0.2:9091", Token: "t2", Ready: true},
		{SandboxID: "sb-4", Backend: "10.0.0.4:9091", Token: "t4", Ready: true},
	}
	rt.Sync(src.ReadyRoutes())
	if _, ok := rt.Lookup("sb-1"); ok {
		t.Error("sb-1 must be GC'd after it leaves the Ready set (terminate)")
	}
	if _, ok := rt.Lookup("sb-2"); !ok {
		t.Error("sb-2 must persist across sync")
	}
	if _, ok := rt.Lookup("sb-4"); !ok {
		t.Error("sb-4 must be added on becoming Ready")
	}
	if rt.Len() != 2 {
		t.Fatalf("after second sync Len=%d want 2", rt.Len())
	}
}
