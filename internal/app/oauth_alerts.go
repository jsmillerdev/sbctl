package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/alerts"
	"github.com/supavise/supavise/internal/oauth"
)

// oauthAlertTimeout bounds one delivery, as upgradeAlertTimeout does.
const oauthAlertTimeout = 90 * time.Second

// oauthAlerter turns what the OAuth service raises (oauth.Service.Alert) into alerts. internal/oauth
// does not import alerts, so the daemon hands the service this hook. The service calls it from the
// token endpoint, on the request that replays a code or a refresh token, so delivery runs on a
// goroutine of its own: a slow webhook must not hold that request up, and a failure to send is
// logged, never the request's.
type oauthAlerter struct {
	notifier *alerts.Notifier
	log      *slog.Logger
	wg       sync.WaitGroup
}

func newOAuthAlerter(n *alerts.Notifier, log *slog.Logger) *oauthAlerter {
	return &oauthAlerter{notifier: n, log: log}
}

// alert is oauth.Service.Alert.
func (a *oauthAlerter) alert(ctx context.Context, e oauth.AlertEvent) {
	ev, ok := oauthAlertEvent(e)
	if !ok {
		a.log.Warn("an OAuth alert of an unknown kind was dropped", "kind", e.Kind)
		return
	}
	// The request may end before the message is out; the message still goes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthAlertTimeout)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		if err := a.notifier.Notify(ctx, ev); err != nil {
			a.log.Warn("could not send the OAuth alert", "kind", ev.Kind, "error", err)
		}
	}()
}

// wait lets the deliveries in flight finish, up to oauthAlertTimeout, when the daemon stops.
func (a *oauthAlerter) wait() {
	done := make(chan struct{})
	go func() { a.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(oauthAlertTimeout):
	}
}

// oauthAlertEvent is the alert for an event of the OAuth service; false for a kind it does not know.
// The key is the service's (one per grant), so the notifier sends a grant's replays once. Title and
// detail are the service's too and carry no secret (oauth.AlertEvent).
func oauthAlertEvent(e oauth.AlertEvent) (alerts.Event, bool) {
	if e.Kind != oauth.AlertKindTokenReuse {
		return alerts.Event{}, false
	}
	return alerts.Event{Kind: alerts.KindOAuthTokenReuse, Severity: alerts.SeverityWarning, Title: e.Title, Detail: e.Detail, Key: e.Key}, true
}
