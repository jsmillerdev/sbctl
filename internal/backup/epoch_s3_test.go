package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestLeaderMarkerS3StoreFake runs the marker scenario against the in-process fake S3 server.
func TestLeaderMarkerS3StoreFake(t *testing.T) { runLeaderMarkerS3(t, fakeS3(t), true) }

// TestLeaderMarkerS3StoreReal runs it against a real S3-compatible service (Garage in CI);
// skipped without SUPAVISE_TEST_S3_*.
func TestLeaderMarkerS3StoreReal(t *testing.T) { runLeaderMarkerS3(t, s3FromEnv(t), false) }

// runLeaderMarkerS3 runs the scenario over o. A strict run expects the service to enforce
// conditional writes atomically (the fake does); a real service is only held to what the marker
// promises either way, and the log says what it enforces.
func runLeaderMarkerS3(t *testing.T, o S3Options, strict bool) {
	ctx := context.Background()
	st, err := NewS3Store(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	cleanS3(t, st)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, err := New(Options{Store: st, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// Whether the service enforces If-None-Match and If-Match, or ignores them (the marker then
	// falls back to a read followed by a plain write, which the design allows).
	probe := "_node/probe-" + fmt.Sprint(time.Now().UnixNano())
	if err := st.PutIf(ctx, probe, []byte("a"), ""); err != nil {
		t.Fatalf("PutIf of a new object: %v", err)
	}
	enforced := errors.Is(st.PutIf(ctx, probe, []byte("b"), ""), ErrPreconditionFailed)
	_, tag, err := st.GetTagged(ctx, probe)
	if err != nil || tag == "" {
		t.Fatalf("GetTagged = %q, %v", tag, err)
	}
	enforcedMatch := errors.Is(st.PutIf(ctx, probe, []byte("c"), `"0123456789abcdef0123456789abcdef"`), ErrPreconditionFailed)
	if err := st.PutIf(ctx, probe, []byte("d"), tag); err != nil && !errors.Is(err, ErrConditionalUnsupported) {
		t.Fatalf("PutIf with the current tag: %v", err)
	}
	t.Logf("store enforces If-None-Match: %v, If-Match: %v", enforced, enforcedMatch)
	_ = st.Delete(ctx, probe)

	if m, err := svc.ReadLeaderMarker(ctx); m != nil || err != nil {
		t.Fatalf("marker of an empty bucket = %+v, %v", m, err)
	}
	if err := svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 2, Leader: "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 3, Leader: "n2"}); err != nil {
		t.Fatal(err)
	}
	m, err := svc.ReadLeaderMarker(ctx)
	if err != nil || m == nil || m.Epoch != 3 || m.Leader != "n2" || !m.At.Equal(now) {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	if _, err := st.Stat(ctx, LeaderMarkerKey); err != nil {
		t.Fatalf("%s is not in the bucket: %v", LeaderMarkerKey, err)
	}
	for _, w := range []LeaderMarker{{Epoch: 2, Leader: "n1"}, {Epoch: 3, Leader: "n1"}} {
		if err := svc.WriteLeaderMarker(ctx, w); !errors.Is(err, ErrMarkerNewer) {
			t.Fatalf("write %+v over epoch 3 by n2 = %v; want ErrMarkerNewer", w, err)
		}
	}

	// Eight nodes promote to epoch 4 at the same moment. One marker wins and the others are told
	// so; with conditional writes enforced there is never a second winner.
	var mu sync.Mutex
	var winners []string
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			leader := fmt.Sprintf("n%d", i+3)
			err := svc.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 4, Leader: leader})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, leader)
			case !errors.Is(err, ErrMarkerNewer):
				t.Errorf("%s: %v", leader, err)
			}
		}()
	}
	wg.Wait()
	m, err = svc.ReadLeaderMarker(ctx)
	if err != nil || m == nil || m.Epoch != 4 {
		t.Fatalf("marker after the race = %+v, %v", m, err)
	}
	if len(winners) == 0 || (strict && len(winners) != 1) {
		t.Fatalf("winners = %v; want one (strict: %v)", winners, strict)
	}
	if !slices.Contains(winners, m.Leader) {
		t.Fatalf("marker names %s, but the winners were %v", m.Leader, winners)
	}
	t.Logf("winners of the race: %v; marker names %s", winners, m.Leader)
}

// A service that does not know If-Match or If-None-Match may refuse the request as a bad request
// instead of answering 501. The marker then falls back to a plain write, as it does for 501, and
// does not stop the failover that writes it.
func TestLeaderMarkerFallsBackWhenTheServiceRefusesTheHeader(t *testing.T) {
	for _, tc := range []struct {
		name, status, code string
	}{
		{"501", "501", "NotImplemented"},
		{"400 InvalidArgument", "400", "InvalidArgument"},
		{"400 MalformedHeader", "400", "MalformedHeader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var body []byte
			var conditional, plain int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && body == nil:
					w.WriteHeader(http.StatusNotFound)
					io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>no</Message></Error>`)
				case r.Method == http.MethodGet:
					w.Header().Set("ETag", `"abc"`)
					w.Write(body)
				case r.Method == http.MethodPut && (r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != ""):
					conditional++
					if tc.status == "501" {
						w.WriteHeader(http.StatusNotImplemented)
					} else {
						w.WriteHeader(http.StatusBadRequest)
					}
					fmt.Fprintf(w, `<Error><Code>%s</Code><Message>unsupported header</Message></Error>`, tc.code)
				case r.Method == http.MethodPut:
					plain++
					body, _ = io.ReadAll(r.Body)
					w.Header().Set("ETag", `"abc"`)
				default:
					http.Error(w, "unexpected "+r.Method, http.StatusTeapot)
				}
			}))
			defer srv.Close()
			st, err := NewS3Store(context.Background(), S3Options{Bucket: "b", Prefix: "p", Endpoint: srv.URL, Region: "us-east-1",
				ForcePathStyle: true, AccessKeyID: "k", SecretKey: "s"})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			ms := MarkerStore(st)
			if err := ms.WriteLeaderMarker(ctx, LeaderMarker{Epoch: 2, Leader: "n1"}); err != nil {
				t.Fatalf("WriteLeaderMarker: %v", err)
			}
			m, err := ms.ReadLeaderMarker(ctx)
			if err != nil || m == nil || m.Epoch != 2 || m.Leader != "n1" {
				t.Fatalf("marker = %+v, %v", m, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if conditional != 1 || plain != 1 {
				t.Fatalf("conditional writes = %d, plain writes = %d; want one of each", conditional, plain)
			}
		})
	}
}
