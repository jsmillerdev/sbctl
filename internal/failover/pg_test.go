package failover

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/supavise/supavise/internal/registry"
)

// The orchestrator runs against the in-memory registry in every other test. These run its main
// flows again against a real Postgres registry (the one CI provides), where SetProjectNode,
// SetLeader and the moves table are SQL: the epoch check, the replica rows, the step log in a jsonb
// column, and a move that is finished again after a resume.

// postgresWorld is newWorld over a registry in a database of its own.
func postgresWorld(t *testing.T) *world {
	t.Helper()
	dsn := os.Getenv("SUPAVISE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	reg, err := registry.Open(context.Background(), privateDatabase(t, dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	return newWorldOn(t, reg)
}

func TestOnPostgres(t *testing.T) {
	if os.Getenv("SUPAVISE_TEST_DATABASE_URL") == "" {
		t.Skip("SUPAVISE_TEST_DATABASE_URL not set")
	}
	for name, fn := range map[string]func(*testing.T){
		"ProjectSwitchover":               TestProjectSwitchover,
		"UnplannedProjectFailover":        TestUnplannedProjectFailover,
		"SwitchoverThatCannotCatchUp":     TestSwitchoverThatCannotCatchUpIsUndone,
		"ServerSwitchover":                TestServerSwitchover,
		"ServerFailover":                  TestServerFailover,
		"PartialProjectFailure":           TestPartialProjectFailureIsReportedAndResumed,
		"RestoreMissing":                  TestRestoreMissingBuildsAStandbyFromTheArchive,
		"EpochRaceStopsBeforePromotion":   TestEpochRaceStopsBeforeThePromotion,
		"ResumeAtEveryStep":               TestResumeAtEveryStep,
		"PlannedUndoneWhenLeaderWontStop": TestPlannedSwitchoverUndoneWhenTheLeaderCannotBeStopped,
		"CutOffByTheRestart":              TestAServerMoveCutOffByTheRestartContinuesInTheDaemonThatStarts,
		"ReadOnlyAfterThePromotion":       TestAServerMoveThatFindsItsRegistryReadOnlyAfterThePromotionWaitsForTheRestart,
		"FollowedFromItsLog":              TestTheMoveIsFollowedFromItsLogWhoeverContinuesIt,
		"AbortBeforeTheMarker":            TestAbortDiscardsAMoveThatStoppedBeforeTheMarkerAndStartsTheLeaderAgain,
		"PausedProject":                   TestAPausedProjectIsSwitchedOverAndStaysPaused,
		"CooldownPerProject":              TestTheCooldownIsPerProjectInProjectModeAndForTheServerInServerMode,
	} {
		t.Run(name, func(t *testing.T) {
			old := newWorldFunc
			newWorldFunc = postgresWorld
			defer func() { newWorldFunc = old }()
			fn(t)
		})
	}
}

// privateDatabase creates a database that only this test uses, drops it when the test ends, and
// returns its DSN: other packages' tests truncate tables in the database the variable names.
func privateDatabase(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "supavise_failover_test_" + hex.EncodeToString(b[:])
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Errorf("dropping %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname=" + name // keyword/value DSN: the last dbname wins
}
