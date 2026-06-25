package console

import (
	"context"
	"sync"
	"time"
)

// ForkNode is one node in an org's live fork tree: a sandbox and its CoW
// relationship to its parent snapshot. PrivateDirtyBytes is the node's unique
// (copied) page set; SharedBytes is the memory it shares with its parent via
// copy-on-write. A node with an empty ParentID is a root snapshot. It carries no
// secret.
type ForkNode struct {
	ID                string    `json:"id"`
	ParentID          string    `json:"parent_id"`
	Phase             string    `json:"phase"`
	PrivateDirtyBytes int64     `json:"private_dirty_bytes"`
	SharedBytes       int64     `json:"shared_bytes"`
	CreatedAt         time.Time `json:"created_at"`
}

// ForkTree is an org's fork forest: the nodes the console renders as the live
// CoW tree. The edges are implied by ParentID.
type ForkTree struct {
	OrgID string     `json:"org_id"`
	Nodes []ForkNode `json:"nodes"`
}

// ForkTreeSource is the org-scoped seam the fork-tree view reads. The REAL
// implementation walks the controller's claim records and the #33 CoW-aware
// metering to fill PrivateDirtyBytes / SharedBytes per node; this slice ships an
// injectable interface and an in-memory fake so the BFF shapes and org-scopes the
// tree now, and the cluster query is a documented follow-up. Tree MUST return
// only the named org's nodes.
type ForkTreeSource interface {
	Tree(ctx context.Context, orgID string) (ForkTree, error)
}

// MemForkTree is the in-memory tested default. It stores per-org node sets and
// never returns one org's nodes to another.
type MemForkTree struct {
	mu    sync.RWMutex
	byOrg map[string][]ForkNode
}

// NewMemForkTree returns an empty in-memory fork-tree source.
func NewMemForkTree() *MemForkTree {
	return &MemForkTree{byOrg: map[string][]ForkNode{}}
}

// Set replaces the node set for one org (test/seed helper).
func (m *MemForkTree) Set(orgID string, nodes []ForkNode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byOrg[orgID] = nodes
}

// Tree returns only the named org's nodes.
func (m *MemForkTree) Tree(_ context.Context, orgID string) (ForkTree, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	nodes := m.byOrg[orgID]
	out := make([]ForkNode, len(nodes))
	copy(out, nodes)
	return ForkTree{OrgID: orgID, Nodes: out}, nil
}
