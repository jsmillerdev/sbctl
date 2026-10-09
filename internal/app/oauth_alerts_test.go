package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/oauth"
)

// The daemon hands this hook to the OAuth service as its Alert callback.
var _ func(context.Context, oauth.AlertEvent) = (*oauthAlerter)(nil).alert

func TestOAuthAlertEventMapping(t *testing.T) {
	ev, ok := oauthAlertEvent(oauth.AlertEvent{Kind: oauth.AlertKindTokenReuse, Key: "oauth_token_reuse/41", Title: "An OAuth token was used twice", Detail: "Grant 41 was revoked."})
	if !ok {
		t.Fatal("the token reuse alert was refused")
	}
	if ev.Kind != alerts.KindOAuthTokenReuse || ev.Severity != alerts.SeverityWarning || ev.Key != "oauth_token_reuse/41" ||
		ev.Title != "An OAuth token was used twice" || ev.Detail != "Grant 41 was revoked." || ev.Ref != "" || ev.Resolved {
		t.Errorf("event = %+v", ev)
	}
	if _, ok := oauthAlertEvent(oauth.AlertEvent{Kind: "something_else", Key: "k"}); ok {
		t.Error("an alert of a kind the daemon does not know was passed on")
	}
}

// The hook delivers on its own goroutine, after the request's context ended, and a grant alerts once
// however often its tokens are replayed.
func TestOAuthAlerterDeliversOncePerGrant(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct{ Kind, Severity, Title string }
		_ = json.Unmarshal(b, &body)
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		sent = append(sent, body.Kind+" "+body.Severity+" "+body.Title)
		mu.Unlock()
	}))
	defer srv.Close()
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Alerts.Webhooks = []config.AlertWebhook{{URL: srv.URL}}
	a := newOAuthAlerter(alerts.New(cfg, alerts.Options{}), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	reuse := func(grant string) oauth.AlertEvent {
		return oauth.AlertEvent{Kind: oauth.AlertKindTokenReuse, Key: oauth.AlertKindTokenReuse + "/" + grant, Title: "An OAuth token was used twice", Detail: "Grant " + grant}
	}
	a.alert(ctx, reuse("41"))
	a.wait() // the second call must see the first one recorded, as it does a second later in the daemon
	a.alert(ctx, reuse("41"))
	a.alert(ctx, reuse("42"))
	cancel() // the token request is gone
	a.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("delivered %v, want one message per grant", sent)
	}
	for _, s := range sent {
		if s != "oauth_token_reuse warning An OAuth token was used twice" {
			t.Errorf("delivered %q", s)
		}
	}
}
