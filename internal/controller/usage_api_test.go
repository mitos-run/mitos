package controller

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// TestUsageAPIRunnableServesOnEveryReplica pins the runnable's leader-election
// posture (issue #602): the internal usage API is a READ surface over the
// shared (durable) usage store, and the chart fronts it with a ClusterIP
// Service selecting EVERY controller replica. A manager runnable that does not
// opt out of leader election only starts on the leader, so the Service would
// round-robin the console into connection refusals on the non-leader replicas.
// The runnable must therefore implement LeaderElectionRunnable and report
// false. The collector (the WRITE side) stays leader-gated.
func TestUsageAPIRunnableServesOnEveryReplica(t *testing.T) {
	var u interface{} = &UsageAPIRunnable{}
	le, ok := u.(manager.LeaderElectionRunnable)
	if !ok {
		t.Fatal("UsageAPIRunnable does not implement manager.LeaderElectionRunnable; it would only serve on the leader replica")
	}
	if le.NeedLeaderElection() {
		t.Fatal("UsageAPIRunnable.NeedLeaderElection() = true; the usage API must serve on every replica behind the chart's Service")
	}
}
