package api

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/lifecycle"
)

// A client that disconnects partway (Ctrl-C on `supabase projects delete`, a closed
// Studio tab, a proxy idle timeout) cancels the request context. Lifecycle mutations
// must not see that cancellation: the Manager is mid-operation and stopping it would
// leave a half-deleted or stopped project.
func TestLifecycleMutationsOutliveTheRequest(t *testing.T) {
	cases := []struct {
		method, path string
		code         int
		ops          []string // Manager calls the endpoint makes, in order
	}{
		{"DELETE", "/v1/projects/%s", 200, []string{"delete"}},
		{"DELETE", "/platform/projects/%s", 200, []string{"delete"}},
		{"POST", "/v1/projects/%s/pause", 200, []string{"pause"}},
		{"POST", "/platform/projects/%s/pause", 201, []string{"pause"}},
		{"POST", "/v1/projects/%s/restore", 200, []string{"resume"}},
		{"POST", "/platform/projects/%s/restore", 200, []string{"resume"}},
		{"POST", "/v1/projects/%s/restart", 200, []string{"pause", "resume"}},
		{"POST", "/platform/projects/%s/restart", 201, []string{"pause", "resume"}},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			f := newFixture(t)
			ref := testRef
			if c.ops[0] == "resume" {
				if err := f.mgr.Pause(context.Background(), ref); err != nil {
					t.Fatal(err)
				}
			}
			reqCtx, cancel := context.WithCancel(context.Background())
			var mu sync.Mutex
			var seen []string
			f.mgr.hook = func(op string, ctx context.Context) {
				cancel() // the client leaves the moment the Manager starts working
				mu.Lock()
				defer mu.Unlock()
				if ctx.Err() != nil {
					seen = append(seen, op+": context already cancelled")
				}
				if _, ok := ctx.Deadline(); !ok {
					seen = append(seen, op+": no deadline, a stuck operation would never end")
				}
			}
			req := httptest.NewRequest(c.method, fmt.Sprintf(c.path, ref), nil).WithContext(reqCtx)
			req.Header.Set("Authorization", "Bearer "+f.jwt)
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, req)
			if rec.Code != c.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.code, rec.Body)
			}
			if len(seen) != 0 {
				t.Fatalf("Manager saw a request-scoped context: %v", seen)
			}
		})
	}
}

func TestRestartRoutesAnswerWithTheSpecStatus(t *testing.T) {
	f := newFixture(t)
	for path, want := range map[string]int{
		"/v1/projects/" + testRef + "/restart":                200,
		"/platform/projects/" + testRef + "/restart-services": 201,
	} {
		if rec := f.do("POST", path, nil); rec.Code != want {
			t.Errorf("%s = %d, want %d: %s", path, rec.Code, want, rec.Body)
		}
	}
	if len(f.mgr.paused) != 2 || len(f.mgr.resumed) != 2 {
		t.Errorf("paused %v resumed %v", f.mgr.paused, f.mgr.resumed)
	}
}

// Studio's login page has fired duplicate creates for one session. Concurrent creates must
// leave exactly one PAT behind: the second used to orphan the first one's token.
func TestConcurrentDeviceLoginCreatesLeaveOnePAT(t *testing.T) {
	f := newFixture(t)
	cli, _ := ecdh.P256().GenerateKey(rand.Reader)
	body := map[string]any{"session_id": "5a1c9e3e-0d52-4d40-9f7a-6f43a1a3c001", "public_key": hex.EncodeToString(cli.PublicKey().Bytes()), "token_name": "cli_race"}
	before, _ := f.reg.ListAccessTokens(context.Background(), f.userID)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := f.do("POST", "/platform/cli/login", body); rec.Code != 201 {
				t.Errorf("create session: %d %s", rec.Code, rec.Body)
			}
		}()
	}
	wg.Wait()
	after, _ := f.reg.ListAccessTokens(context.Background(), f.userID)
	if got := len(after) - len(before); got != 1 {
		t.Fatalf("%d tokens were left for one session, want 1", got)
	}
}

func TestClientCancelIsNot5xx(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/platform/pg-meta/"+testRef+"/query", strings.NewReader(`{"query":"select 1"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.jwt)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 499 {
		t.Fatalf("cancelled request answered %d %s, want 499", rec.Code, rec.Body)
	}
}

// A shutdown waits for the lifecycle operations that outlive their request, and refuses
// new ones with 503 while it waits.
func TestDrainWaitsForInFlightOperationsAndRefusesNewOnes(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.mgr.hook = func(op string, _ context.Context) {
		if op == "delete" {
			close(entered)
			<-release
		}
	}
	served := make(chan int, 1)
	go func() { served <- f.do("DELETE", "/v1/projects/"+testRef, nil).Code }()
	<-entered

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- f.srv.Drain(ctx)
	}()
	// Drain has begun once new mutations are refused.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rec := f.do("POST", "/v1/projects/"+testRef+"/pause", nil); rec.Code == 503 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("a mutation during Drain answered %d, want 503", rec.Code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) while a delete was running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-drained; err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if code := <-served; code != 200 {
		t.Fatalf("the in-flight delete answered %d, want 200", code)
	}
	if len(f.mgr.deleted) != 1 {
		t.Fatalf("deleted = %v", f.mgr.deleted)
	}
}

func TestDrainGivesUpAtItsDeadline(t *testing.T) {
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.mgr.hook = func(op string, _ context.Context) {
		if op == "pause" {
			close(entered)
			<-release
		}
	}
	go f.do("POST", "/v1/projects/"+testRef+"/pause", nil)
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := f.srv.Drain(ctx)
	if err == nil || !strings.Contains(err.Error(), "1 lifecycle operation") {
		t.Fatalf("Drain = %v, want an error naming the operation still running", err)
	}
}

// restart records its intent, so a daemon stop between pause and resume can be undone.
func TestRestartRecordsItsIntent(t *testing.T) {
	f := newFixture(t)
	if rec := f.do("POST", "/v1/projects/"+testRef+"/restart", nil); rec.Code != 200 {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body)
	}
	evs, _ := f.reg.ListEvents(context.Background(), testRef, 20)
	var kinds []string
	for _, e := range evs { // newest first
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) < 2 || kinds[0] != lifecycle.EventRestartFinished || kinds[len(kinds)-1] != lifecycle.EventRestartRequested {
		t.Fatalf("events (newest first) = %v, want restart_finished ... restart_requested", kinds)
	}
}
