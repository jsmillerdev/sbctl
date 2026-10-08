package main

import (
	"context"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/nodeupgrade"
	"github.com/jsmillerdev/supavise/internal/registry"
)

// The history behind a second rollback: v1 -> v2 moves a project (window 1), v2 -> v3 moves it
// again (window 2), and the rollback from v3 puts it back with an upgrade of its own. The latest
// upgrade of the project is then that revert, so only the windows tell the moves of each release.
func TestMovesBetweenFindsWhatEachReleaseMoved(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	org, err := reg.CreateOrganization(ctx, "acme", "Acme")
	if err != nil {
		t.Fatal(err)
	}
	const ref = "aaaaaaaaaaaaaaaaaaaa"
	if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, OrgID: org.ID, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	put := func(id string, at time.Time, from, to string) {
		t.Helper()
		u := &registry.Upgrade{TrackingID: id, Ref: ref, Status: registry.UpgradeDone, InitiatedAt: at, LatestStatusAt: at,
			From: map[string]string{"gotrue": from}, To: map[string]string{"gotrue": to}}
		if err := reg.PutUpgrade(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	put("11111111-1111-4111-8111-111111111111", day, "auth-v1", "auth-v2")                   // upgrade to v2
	put("22222222-2222-4222-8222-222222222222", day.Add(24*time.Hour), "auth-v2", "auth-v3") // upgrade to v3
	put("33333333-3333-4333-8333-333333333333", day.Add(48*time.Hour), "auth-v3", "auth-v2") // rollback to v2
	moves := func(since, until time.Time) []nodeupgrade.ProjectMove {
		t.Helper()
		ups, err := reg.UpgradesBetween(ctx, since, until)
		if err != nil {
			t.Fatal(err)
		}
		return netMoves(ups)
	}
	// The rollback from v3 reads v3's window.
	got := moves(day.Add(23*time.Hour), day.Add(25*time.Hour))
	if len(got) != 1 || got[0].From["gotrue"] != "auth-v2" || got[0].To["gotrue"] != "auth-v3" {
		t.Fatalf("moves of v3 = %+v", got)
	}
	// The second rollback, from v2, reads v2's window, which the first one did not change: the
	// project is still on v2's release, so it goes back to v1's.
	got = moves(day.Add(-time.Hour), day.Add(time.Hour))
	if len(got) != 1 || got[0].From["gotrue"] != "auth-v1" || got[0].To["gotrue"] != "auth-v2" {
		t.Fatalf("moves of v2 = %+v", got)
	}
	// With no window end, a run's own moves fold into one: v1 -> v3 -> v2 is a project that ran v1 and runs v2.
	got = moves(day.Add(-time.Hour), time.Time{})
	if len(got) != 1 || got[0].From["gotrue"] != "auth-v1" || got[0].To["gotrue"] != "auth-v2" {
		t.Fatalf("folded moves = %+v", got)
	}
	// An upgrade that ended where it began moves nothing.
	put("44444444-4444-4444-8444-444444444444", day.Add(72*time.Hour), "auth-v2", "auth-v3")
	put("55555555-5555-4555-8555-555555555555", day.Add(73*time.Hour), "auth-v3", "auth-v2")
	if got = moves(day.Add(71*time.Hour), time.Time{}); len(got) != 0 {
		t.Fatalf("a round trip is a move: %+v", got)
	}
}
