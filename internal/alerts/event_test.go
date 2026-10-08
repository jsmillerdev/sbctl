package alerts

import (
	"context"
	"testing"

	"github.com/supavise/supavise/internal/config"
)

// The kinds of the replica and failover work: the failover ones are announcements, the rest are
// conditions, and the checker raises none of them.
func TestClusterKinds(t *testing.T) {
	announcements := []string{KindFailoverStarted, KindFailoverCompleted, KindFailoverFailed}
	conditions := []string{
		KindReplicaUnhealthy, KindReplicaLag, KindReplicaNeedsRebuild, KindReplicaCapacity, KindNodeUnreachable,
		KindNodeVersionSkew, KindFenced, KindInfraBehind, KindHostNotConverged, KindStandbyBehind, KindStorageNotS3,
	}
	seen := map[string]bool{}
	for _, k := range append(append([]string{}, announcements...), conditions...) {
		if k == "" || seen[k] {
			t.Errorf("kind %q is empty or listed twice", k)
		}
		seen[k] = true
		if owned(k) {
			t.Errorf("%s: the checker would resolve a condition it never raises", k)
		}
	}
	for _, k := range announcements {
		if !oneShot(k) {
			t.Errorf("%s should be an announcement", k)
		}
	}
	for _, k := range conditions {
		if oneShot(k) {
			t.Errorf("%s should be a condition", k)
		}
	}

	s := newSink(t)
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := n.Notify(ctx, Event{Kind: KindFailoverFailed, Severity: SeverityCritical, Title: "Failover failed"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := n.Notify(ctx, Event{Kind: KindReplicaUnhealthy, Ref: "aaaaaaaaaaaaaaaaaaaa", Title: "Replica is not healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 4 {
		t.Errorf("%d notifications: three failed failovers and one standing replica problem make 4", s.count())
	}
	if err := n.Notify(ctx, Event{Kind: KindReplicaUnhealthy, Ref: "aaaaaaaaaaaaaaaaaaaa", Title: "Replica is not healthy", Resolved: true}); err != nil || s.count() != 5 {
		t.Errorf("recovery: %d, %v", s.count(), err)
	}
}
