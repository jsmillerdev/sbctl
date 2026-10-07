package alerts

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// sink is a webhook endpoint that records what it is sent.
type sink struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
	status int
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{status: 200}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.bodies = append(s.bodies, b)
		s.heads = append(s.heads, r.Header.Clone())
		w.WriteHeader(s.status)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.bodies) }

func (s *sink) setStatus(code int) { s.mu.Lock(); s.status = code; s.mu.Unlock() }

func (s *sink) last(t *testing.T) (webhookBody, http.Header, []byte) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("the endpoint received nothing")
	}
	var b webhookBody
	raw := s.bodies[len(s.bodies)-1]
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	return b, s.heads[len(s.heads)-1], raw
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)} }

func testCfg(t *testing.T, hooks ...config.AlertWebhook) *config.Config {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.Domain = "node.example.com"
	cfg.Alerts.Webhooks = hooks
	return cfg
}

func TestWebhookBodyAndSignature(t *testing.T) {
	s := newSink(t)
	cfg := testCfg(t, config.AlertWebhook{URL: s.srv.URL + "/hook/secret-part", Secret: "s3cret"})
	clk := newClock()
	n := New(cfg, Options{Now: clk.now})

	err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Severity: SeverityCritical, Title: "Disk space is low", Detail: "3% free", Ref: ""})
	if err != nil {
		t.Fatal(err)
	}
	body, head, raw := s.last(t)
	if body.Kind != "disk_low" || body.Severity != "critical" || body.Title != "Disk space is low" || body.Detail != "3% free" ||
		body.Node != "node.example.com" || body.Version != 1 || body.Resolved || body.Time != "2026-10-07T12:00:00Z" {
		t.Errorf("body %+v", body)
	}
	if !strings.Contains(body.Text, "[critical] node.example.com: Disk space is low") || !strings.Contains(body.Text, "3% free") {
		t.Errorf("text %q", body.Text)
	}
	if head.Get("Content-Type") != "application/json" || head.Get(EventHeader) != "disk_low" {
		t.Errorf("headers %v", head)
	}
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write(raw)
	if want := "sha256=" + hex.EncodeToString(m.Sum(nil)); head.Get(SignatureHeader) != want {
		t.Errorf("signature %q, want %q", head.Get(SignatureHeader), want)
	}
	if Sign("s3cret", raw) != head.Get(SignatureHeader) {
		t.Error("Sign disagrees with the header")
	}
}

func TestUnsignedWebhookHasNoSignatureHeader(t *testing.T) {
	s := newSink(t)
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{})
	if err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, head, _ := s.last(t); head.Get(SignatureHeader) != "" {
		t.Errorf("an unsigned webhook carries %q", head.Get(SignatureHeader))
	}
}

func TestDuplicatesAreHeldUntilTheRepeatInterval(t *testing.T) {
	s := newSink(t)
	clk := newClock()
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{Now: clk.now})
	ev := Event{Kind: KindProjectUnhealthy, Ref: "aaaaaaaaaaaaaaaaaaaa", Title: "Project is not healthy"}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := n.Notify(ctx, ev); err != nil {
			t.Fatal(err)
		}
		clk.add(time.Minute)
	}
	if s.count() != 1 {
		t.Fatalf("%d notifications for one standing problem", s.count())
	}
	// Another project is another problem.
	other := ev
	other.Ref = "bbbbbbbbbbbbbbbbbbbb"
	if err := n.Notify(ctx, other); err != nil || s.count() != 2 {
		t.Fatalf("a different project: %d, %v", s.count(), err)
	}
	// Still wrong after the repeat interval: a reminder.
	clk.add(13 * time.Hour)
	if err := n.Notify(ctx, ev); err != nil || s.count() != 3 {
		t.Errorf("the reminder: %d, %v", s.count(), err)
	}
	// An explicit key overrides Kind and Ref.
	a, b := Event{Kind: KindBackupFailed, Key: "k1", Title: "x"}, Event{Kind: KindBackupFailed, Key: "k2", Title: "x"}
	_ = n.Notify(ctx, a)
	_ = n.Notify(ctx, b)
	_ = n.Notify(ctx, a)
	if s.count() != 5 {
		t.Errorf("keys: %d notifications, want 5", s.count())
	}
}

func TestDedupeSurvivesARestartAndIsSharedBetweenProcesses(t *testing.T) {
	s := newSink(t)
	cfg := testCfg(t, config.AlertWebhook{URL: s.srv.URL})
	clk := newClock()
	ev := Event{Kind: KindDiskLow, Title: "Disk space is low"}
	if err := New(cfg, Options{Now: clk.now}).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	// A new Notifier over the same state directory is the daemon after a restart, or a CLI run.
	if err := New(cfg, Options{Now: clk.now}).Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if s.count() != 1 {
		t.Errorf("%d notifications across two notifiers", s.count())
	}
}

func TestAnnouncementsAreNotDeduplicated(t *testing.T) {
	s := newSink(t)
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{})
	for i := 0; i < 3; i++ {
		if err := n.Notify(context.Background(), Event{Kind: KindUpgradeFailed, Severity: SeverityCritical, Title: "Upgrade failed", Detail: fmt.Sprint("attempt ", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 3 {
		t.Errorf("%d notifications for three failed upgrades", s.count())
	}
	if act, _ := n.Active(); len(act) != 0 {
		t.Errorf("an announcement became an active problem: %+v", act)
	}
}

func TestRecoveryIsSentOnceAndOnlyForAProblemThatWasSent(t *testing.T) {
	s := newSink(t)
	clk := newClock()
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{Now: clk.now})
	ctx := context.Background()
	ev := Event{Kind: KindDiskLow, Severity: SeverityWarning, Title: "Disk space is low", Key: "disk_low/"}

	rec := ev
	rec.Resolved = true
	if err := n.Notify(ctx, rec); err != nil || s.count() != 0 {
		t.Fatalf("a recovery for a problem nobody was told about was sent: %d %v", s.count(), err)
	}
	_ = n.Notify(ctx, ev)
	if act, _ := n.Active(); len(act) != 1 || act["disk_low/"].Title != "Disk space is low" {
		t.Fatalf("active: %+v", act)
	}
	if err := n.Notify(ctx, rec); err != nil || s.count() != 2 {
		t.Fatalf("the recovery: %d %v", s.count(), err)
	}
	if body, _, _ := s.last(t); !body.Resolved || !strings.Contains(body.Text, "(resolved)") {
		t.Errorf("recovery body %+v", body)
	}
	_ = n.Notify(ctx, rec)
	if s.count() != 2 {
		t.Errorf("the recovery was sent twice")
	}
	if act, _ := n.Active(); len(act) != 0 {
		t.Errorf("active after recovery: %+v", act)
	}
	// After the recovery the problem can be raised again at once.
	_ = n.Notify(ctx, ev)
	if s.count() != 3 {
		t.Errorf("a problem that came back was not sent: %d", s.count())
	}
}

func TestHourlyCapHoldsBackAndSaysSo(t *testing.T) {
	s := newSink(t)
	clk := newClock()
	cfg := testCfg(t, config.AlertWebhook{URL: s.srv.URL})
	cfg.Alerts.MaxPerHour = 3
	n := New(cfg, Options{Now: clk.now})
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		ev := Event{Kind: KindProjectUnhealthy, Ref: fmt.Sprintf("%020d", i), Title: "Project is not healthy"}
		if err := n.Notify(ctx, ev); err != nil {
			t.Fatal(err)
		}
		clk.add(time.Second)
	}
	if s.count() != 3 {
		t.Fatalf("%d notifications under a cap of 3", s.count())
	}
	// A held alert asked for again (the checker does, each cycle) is counted once.
	for i := 0; i < 4; i++ {
		_ = n.Notify(ctx, Event{Kind: KindProjectUnhealthy, Ref: fmt.Sprintf("%020d", 3), Title: "Project is not healthy"})
	}
	// An hour later the window is open again, and the next one says what was held back.
	clk.add(time.Hour)
	if err := n.Notify(ctx, Event{Kind: KindDiskLow, Title: "Disk space is low"}); err != nil {
		t.Fatal(err)
	}
	body, _, _ := s.last(t)
	if s.count() != 4 || !strings.Contains(body.Detail, "3 earlier alert(s) were held back") {
		t.Errorf("count %d, detail %q", s.count(), body.Detail)
	}
	clk.add(time.Second)
	_ = n.Notify(ctx, Event{Kind: KindBackupFailed, Ref: "x", Title: "t"})
	if body, _, _ := s.last(t); strings.Contains(body.Detail, "held back") {
		t.Errorf("the note repeats: %q", body.Detail)
	}
}

func TestMinSeverity(t *testing.T) {
	s := newSink(t)
	cfg := testCfg(t, config.AlertWebhook{URL: s.srv.URL})
	cfg.Alerts.MinSeverity = "warning"
	n := New(cfg, Options{})
	_ = n.Notify(context.Background(), Event{Kind: KindUpdateAvailable, Severity: SeverityInfo, Title: "v2 is available"})
	if s.count() != 0 {
		t.Error("an info alert passed min_severity = warning")
	}
	_ = n.Notify(context.Background(), Event{Kind: KindDiskLow, Severity: SeverityWarning, Title: "t"})
	if s.count() != 1 {
		t.Error("a warning was dropped")
	}
}

func TestFailedDeliveryIsNotRecordedSoTheNextTrySends(t *testing.T) {
	s := newSink(t)
	s.setStatus(400) // a client error is not retried inside one Notify
	n := New(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), Options{})
	ev := Event{Kind: KindDiskLow, Title: "Disk space is low"}
	if err := n.Notify(context.Background(), ev); err == nil {
		t.Fatal("a rejected delivery returned no error")
	}
	if act, _ := n.Active(); len(act) != 0 {
		t.Errorf("a failed delivery was recorded as sent: %+v", act)
	}
	s.setStatus(200)
	if err := n.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if act, _ := n.Active(); len(act) != 1 {
		t.Errorf("active: %+v", act)
	}
}

func TestOneGoodDestinationIsEnough(t *testing.T) {
	good, bad := newSink(t), newSink(t)
	bad.setStatus(400)
	n := New(testCfg(t, config.AlertWebhook{URL: bad.srv.URL}, config.AlertWebhook{URL: good.srv.URL}), Options{})
	if err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Title: "t"}); err != nil {
		t.Errorf("one failing destination failed the alert: %v", err)
	}
	if good.count() != 1 {
		t.Error("the working destination was not reached")
	}
}

func TestWebhookRetriesAServerErrorOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n := New(testCfg(t, config.AlertWebhook{URL: srv.URL}), Options{})
	if err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("%d calls", calls)
	}
}

func TestNothingConfiguredLogsAndSendsNothing(t *testing.T) {
	n := New(testCfg(t), Options{})
	if n.Configured() {
		t.Error("Configured")
	}
	if err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Title: "t"}); err != nil {
		t.Errorf("an alert with no destination returned %v", err)
	}
	if act, _ := n.Active(); len(act) != 0 {
		t.Errorf("recorded without a destination: %+v", act)
	}
	if _, err := n.Test(context.Background()); err == nil {
		t.Error("Test with no destination did not say so")
	}
	if err := n.Notify(context.Background(), Event{}); err == nil {
		t.Error("an event without a kind was accepted")
	}
}

func TestTestAlertIgnoresDedupeAndCapAndReportsEachDestination(t *testing.T) {
	good, bad := newSink(t), newSink(t)
	bad.setStatus(403)
	cfg := testCfg(t, config.AlertWebhook{URL: good.srv.URL + "/a/path/with/a/secret"}, config.AlertWebhook{URL: bad.srv.URL})
	cfg.Alerts.MaxPerHour = 1
	n := New(cfg, Options{})
	for i := 0; i < 3; i++ {
		res, err := n.Test(context.Background())
		if err != nil || len(res) != 2 {
			t.Fatalf("%v %v", res, err)
		}
		if res[0].Err != nil || res[1].Err == nil {
			t.Errorf("results %+v", res)
		}
		if strings.Contains(res[0].Destination, "secret") {
			t.Errorf("the destination name shows the secret part of the URL: %s", res[0].Destination)
		}
	}
	if good.count() != 3 {
		t.Errorf("%d test alerts", good.count())
	}
	if body, _, _ := good.last(t); body.Kind != "test" || body.Severity != "info" {
		t.Errorf("%+v", body)
	}
}

func TestPackageLevelNotify(t *testing.T) {
	SetDefault(nil)
	if err := Notify(context.Background(), Event{Kind: KindUpgradeStarted, Title: "x"}); err != nil {
		t.Errorf("Notify without a default returned %v", err)
	}
	s := newSink(t)
	Configure(testCfg(t, config.AlertWebhook{URL: s.srv.URL}), nil)
	t.Cleanup(func() { SetDefault(nil) })
	if err := Notify(context.Background(), Event{Kind: KindUpgradeStarted, Severity: SeverityInfo, Title: "Upgrade started", Detail: "v1 to v2"}); err != nil {
		t.Fatal(err)
	}
	if body, _, _ := s.last(t); body.Kind != "upgrade_started" {
		t.Errorf("%+v", body)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.slack.com/services/T000/B000/XXXX": "https://hooks.slack.com",
		"https://example.com:8443/h?token=abc":            "https://example.com:8443",
		"https://example.com":                             "https://example.com",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// ---- mail ----

type capturedMail struct {
	to      []string
	subject string
	text    string
}

func TestEmailGoesThroughTheMailer(t *testing.T) {
	var got []capturedMail
	cfg := testCfg(t)
	cfg.Mail = config.Mail{SMTPHost: "smtp.example.com", SMTPFrom: "ops@example.com"}
	cfg.Alerts.EmailTo = "a@example.com, b@example.com"
	n := New(cfg, Options{Mailer: func(_ context.Context, to []string, subject, text string) error {
		got = append(got, capturedMail{to, subject, text})
		return nil
	}})
	if err := n.Notify(context.Background(), Event{Kind: KindBackupFailed, Severity: SeverityWarning, Ref: "abc", Title: "Backup failed", Detail: "S3 said no"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].to) != 2 || got[0].to[1] != "b@example.com" {
		t.Fatalf("%+v", got)
	}
	if got[0].subject != "[Supavise] warning: node.example.com: Backup failed" {
		t.Errorf("subject %q", got[0].subject)
	}
	for _, want := range []string{"Backup failed", "S3 said no", "Project: abc", "Kind: backup_failed", "Node: node.example.com"} {
		if !strings.Contains(got[0].text, want) {
			t.Errorf("body lacks %q:\n%s", want, got[0].text)
		}
	}
}

func TestComposeMailCannotInjectHeaders(t *testing.T) {
	msg := composeMail("Supavise <ops@example.com>", []string{"a@example.com"}, "Backup of x\r\nBcc: evil@example.com", "body\nline2", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	head, body, _ := strings.Cut(msg, "\r\n\r\n")
	if strings.Contains(head, "\r\nBcc:") {
		t.Errorf("header injection:\n%s", head)
	}
	if !strings.Contains(head, "Content-Type: text/plain; charset=utf-8") || !strings.Contains(body, "body\r\nline2") {
		t.Errorf("message:\n%s", msg)
	}
}

// fakeSMTP is just enough of an SMTP server for net/smtp: no TLS, no AUTH, one message.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	from string
	rcpt []string
	data string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { fmt.Fprint(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			f.mu.Lock()
			f.from = strings.Trim(strings.TrimSpace(line[len("MAIL FROM:"):]), "<>")
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>"))
			f.mu.Unlock()
			say("250 ok")
		case cmd == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestSMTPDelivery(t *testing.T) {
	f := newFakeSMTP(t)
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	var p int
	fmt.Sscan(port, &p)
	cfg := testCfg(t)
	cfg.Mail = config.Mail{SMTPHost: host, SMTPPort: p, SMTPFrom: "ops@example.com", SMTPName: "Node ops"}
	cfg.Alerts.EmailTo = "me@example.com"
	n := New(cfg, Options{})
	if err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Severity: SeverityCritical, Title: "Disk space is low", Detail: "2% free"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.from != "ops@example.com" || len(f.rcpt) != 1 || f.rcpt[0] != "me@example.com" {
		t.Errorf("envelope from %q to %v", f.from, f.rcpt)
	}
	for _, want := range []string{"From: \"Node ops\" <ops@example.com>", "To: me@example.com", "Subject: [Supavise] critical: node.example.com: Disk space is low", "2% free"} {
		if !strings.Contains(f.data, want) {
			t.Errorf("message lacks %q:\n%s", want, f.data)
		}
	}
}

func TestSMTPFailureIsReported(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens
	host, port, _ := net.SplitHostPort(addr)
	var p int
	fmt.Sscan(port, &p)
	cfg := testCfg(t)
	cfg.Mail = config.Mail{SMTPHost: host, SMTPPort: p, SMTPFrom: "ops@example.com"}
	cfg.Alerts.EmailTo = "me@example.com"
	n := New(cfg, Options{})
	if err := n.Notify(context.Background(), Event{Kind: KindDiskLow, Title: "t"}); err == nil {
		t.Fatal("a refused SMTP connection returned no error")
	}
	if act, _ := n.Active(); len(act) != 0 {
		t.Error("recorded as sent")
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*config.Config)
		bad  bool
	}{
		{"defaults", func(*config.Config) {}, false},
		{"https webhook", func(c *config.Config) {
			c.Alerts.Webhooks = []config.AlertWebhook{{URL: "https://hooks.example.com/x"}}
		}, false},
		{"not a URL", func(c *config.Config) { c.Alerts.Webhooks = []config.AlertWebhook{{URL: "hooks.example.com"}} }, true},
		{"ftp", func(c *config.Config) { c.Alerts.Webhooks = []config.AlertWebhook{{URL: "ftp://x/y"}} }, true},
		{"email without mail", func(c *config.Config) { c.Alerts.EmailTo = "a@b.c" }, true},
		{"email with mail", func(c *config.Config) {
			c.Alerts.EmailTo = "a@b.c"
			c.Mail = config.Mail{SMTPHost: "h", SMTPFrom: "f@x.y"}
		}, false},
		{"bad severity", func(c *config.Config) { c.Alerts.MinSeverity = "loud" }, true},
		{"negative cap", func(c *config.Config) { c.Alerts.MaxPerHour = -1 }, true},
		{"bad disk percent", func(c *config.Config) { c.Health.DiskLowPercent = 95 }, true},
	}
	for _, c := range cases {
		cfg := config.Default()
		c.mut(cfg)
		if err := cfg.Validate(); (err != nil) != c.bad {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
