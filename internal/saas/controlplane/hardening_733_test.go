package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
)

// TestCreateRejectsReplicasAboveCap asserts a create naming a replicas count
// above maxCreateReplicas is refused at request validation with a 400
// invalid_input and builds no Sandbox, so a huge value cannot invite controller
// churn (issue #733, item 4).
func TestCreateRejectsReplicasAboveCap(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second))

	body := fmt.Sprintf(`{"pool":"default","replicas":%d}`, maxCreateReplicas+1)
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(body),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "invalid_input") {
		t.Errorf("error is not shaped as invalid_input: %s", resp.Body)
	}
	var list v1.SandboxList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Errorf("created %d sandboxes for an over-cap replicas request", len(list.Items))
	}
}

// TestCreateAcceptsReplicasAtCap asserts a create at exactly the cap is accepted
// (the cap is inclusive) and stamps the replicas onto the Sandbox spec.
func TestCreateAcceptsReplicasAtCap(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"))
	cp := New(c, WithPollInterval(5*time.Millisecond), WithReadyTimeout(20*time.Millisecond))

	body := fmt.Sprintf(`{"pool":"default","replicas":%d}`, maxCreateReplicas)
	// The create polls to a terminal outcome; with no controller flipping the
	// phase it times out, but the Sandbox is built first, which is what we assert.
	_, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(body),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	var list v1.SandboxList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("built %d sandboxes, want 1", len(list.Items))
	}
	if got := list.Items[0].Spec.Replicas; got != maxCreateReplicas {
		t.Errorf("replicas = %d, want %d", got, maxCreateReplicas)
	}
}

// TestListTemplatesScopedToCallerNamespace asserts template.list returns only
// pools in the caller's own namespace, so a tenant cannot enumerate another
// tenant's pool names and readiness (issue #733, item 5).
func TestListTemplatesScopedToCallerNamespace(t *testing.T) {
	c := newFakeClient(t, poolIn(orgA, "default"), poolIn(orgB, "secret-pool"))
	cp := New(c)

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "template.list"})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.Status, resp.Body)
	}
	var got []templateDescriptor
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("unmarshal: %v; body = %s", err, resp.Body)
	}
	names := map[string]bool{}
	for _, d := range got {
		names[d.ID] = true
	}
	if !names["default"] {
		t.Errorf("caller's own pool 'default' missing from list: %v", names)
	}
	if names["secret-pool"] {
		t.Errorf("another tenant's pool 'secret-pool' leaked into the list: %v", names)
	}
}
