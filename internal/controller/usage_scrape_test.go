package controller

import (
	"testing"
	"time"
)

// TestRegistryNodeListerExposesHTTPEndpoints asserts the NodeLister adapter yields
// each registered node's name and HTTP endpoint (the forkd /v1/metering target),
// and skips a node with no HTTP endpoint. It exposes only the name and HTTP
// endpoint, never the gRPC/CAS endpoints or capacity, so the usage package sees
// the minimal surface and there is no import cycle.
func TestRegistryNodeListerExposesHTTPEndpoints(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{Name: "n1", Endpoint: "10.0.0.1:9090", HTTPEndpoint: "10.0.0.1:9091", LastHeartbeat: time.Now()})
	reg.Register(&NodeInfo{Name: "n2", Endpoint: "10.0.0.2:9090", HTTPEndpoint: "10.0.0.2:9091", LastHeartbeat: time.Now()})
	// A node with no HTTP endpoint must be skipped (nothing to scrape).
	reg.Register(&NodeInfo{Name: "n3", Endpoint: "10.0.0.3:9090", LastHeartbeat: time.Now()})

	lister := RegistryNodeLister{Registry: reg}
	got := lister.ListNodeEndpoints()

	byName := map[string]string{}
	for _, e := range got {
		byName[e.Name] = e.HTTPEndpoint
	}
	if len(byName) != 2 {
		t.Fatalf("want 2 endpoints (n3 has no HTTP endpoint), got %d: %v", len(byName), byName)
	}
	if byName["n1"] != "10.0.0.1:9091" {
		t.Errorf("n1 endpoint = %q, want 10.0.0.1:9091", byName["n1"])
	}
	if byName["n2"] != "10.0.0.2:9091" {
		t.Errorf("n2 endpoint = %q, want 10.0.0.2:9091", byName["n2"])
	}
	if _, ok := byName["n3"]; ok {
		t.Errorf("n3 (no HTTP endpoint) must be skipped, got %q", byName["n3"])
	}
}
