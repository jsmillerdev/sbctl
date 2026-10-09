package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/oauth"
)

// The OAuth service names its alert by a string of its own (it cannot import this package), and the
// notifier de-duplicates by the key it builds. Both must agree with this package.
func TestOAuthTokenReuseKindMatchesTheService(t *testing.T) {
	if KindOAuthTokenReuse != oauth.AlertKindTokenReuse {
		t.Fatalf("alerts.KindOAuthTokenReuse = %q, oauth.AlertKindTokenReuse = %q", KindOAuthTokenReuse, oauth.AlertKindTokenReuse)
	}
	// A condition, not an announcement: the key is what makes it fire once per grant.
	if oneShot(KindOAuthTokenReuse) {
		t.Error("oauth_token_reuse must be a condition, so that the key de-duplicates it")
	}
	// The checker resolves the conditions it owns when the health report no longer shows them; this one
	// is not in a health report, and a resolution would send a false recovery.
	if owned(KindOAuthTokenReuse) {
		t.Error("the checker must not own oauth_token_reuse")
	}
}

// A replayed refresh token or code arrives once per attempt: the same grant alerts once, another
// grant alerts on its own, and the alert comes back after [alerts] repeat_hours if the replays go on.
func TestOAuthTokenReuseAlertsOncePerGrant(t *testing.T) {
	s := newSink(t)
	clk := newClock()
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{Now: clk.now})
	ctx := context.Background()
	reuse := func(grant string) Event {
		return Event{Kind: KindOAuthTokenReuse, Severity: SeverityWarning, Title: "An OAuth token was used twice",
			Detail: "Grant " + grant + " was revoked.", Key: oauth.AlertKindTokenReuse + "/" + grant}
	}
	for i := 0; i < 5; i++ {
		if err := n.Notify(ctx, reuse("41")); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 1 {
		t.Fatalf("%d notifications for five replays on one grant, want 1", s.count())
	}
	body, head, _ := s.last(t)
	if body.Kind != "oauth_token_reuse" || body.Severity != SeverityWarning || body.Ref != "" || head.Get("X-Supavise-Event") != "oauth_token_reuse" {
		t.Errorf("body %+v, event header %q", body, head.Get("X-Supavise-Event"))
	}

	if err := n.Notify(ctx, reuse("42")); err != nil {
		t.Fatal(err)
	}
	if s.count() != 2 {
		t.Fatalf("%d notifications after a second grant, want 2", s.count())
	}

	active, err := n.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active["oauth_token_reuse/41"].Kind != KindOAuthTokenReuse {
		t.Errorf("active alerts %+v, want one per grant", active)
	}

	clk.add(config.Default().Alerts.Repeat() + time.Minute)
	if err := n.Notify(ctx, reuse("41")); err != nil {
		t.Fatal(err)
	}
	if s.count() != 3 {
		t.Errorf("%d notifications after the repeat interval, want the reminder (3)", s.count())
	}
}
