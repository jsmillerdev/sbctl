package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/jsmillerdev/supavise/internal/config"
)

// SignatureHeader carries the HMAC of the body of a webhook that has a secret:
// "sha256=" and the hex HMAC-SHA256 of the request body under the secret.
const SignatureHeader = "X-Supavise-Signature"

// EventHeader carries the alert's kind, for receivers that route on it.
const EventHeader = "X-Supavise-Event"

// webhookBody is what a webhook receives. Text is a ready sentence: Slack, Mattermost and
// Rocket.Chat incoming webhooks show it as the message, and anything else can ignore it.
type webhookBody struct {
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Resolved bool   `json:"resolved"`
	Node     string `json:"node"`
	Time     string `json:"time"`
	Text     string `json:"text"`
}

func (n *Notifier) body(ev Event) webhookBody {
	title := ev.Title
	if ev.Resolved {
		title += " (resolved)"
	}
	text := fmt.Sprintf("[%s] %s: %s", ev.severity(), n.node, title)
	if ev.Detail != "" {
		text += "\n" + ev.Detail
	}
	return webhookBody{
		Version: 1, Kind: ev.Kind, Severity: ev.severity(), Title: ev.Title, Detail: ev.Detail, Ref: ev.Ref,
		Resolved: ev.Resolved, Node: n.node, Time: ev.Time.UTC().Format(time.RFC3339), Text: text,
	}
}

// Sign returns the SignatureHeader value for body under secret.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// postWebhook sends one alert to one endpoint, and retries once on a failure that can heal.
func (n *Notifier) postWebhook(ctx context.Context, w config.AlertWebhook, ev Event) error {
	b, err := json.Marshal(n.body(ev))
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(w.URL), bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "supavise-alerts")
		req.Header.Set(EventHeader, ev.Kind)
		if w.Secret != "" {
			req.Header.Set(SignatureHeader, Sign(w.Secret, b))
		}
		resp, err := n.http.Do(req)
		if err != nil {
			last = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == 429 || resp.StatusCode >= 500:
			last = fmt.Errorf("answered %s", resp.Status)
		default:
			return fmt.Errorf("answered %s", resp.Status)
		}
	}
	return last
}

// redactURL names a webhook in logs and errors without its path or query, where chat services
// keep the secret part of the address.
func redactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?"); j >= 0 {
			rest = rest[:j]
		}
		return raw[:i+3] + rest
	}
	return "webhook"
}

// sendMail sends one message through the [mail] relay: STARTTLS when the server offers it
// (implicit TLS on port 465), PLAIN authentication when smtp_user is set.
func (n *Notifier) sendMail(ctx context.Context, to []string, subject, text string) error {
	if n.mailer != nil {
		return n.mailer(ctx, to, subject, text)
	}
	m := n.mail
	port := m.SMTPPort
	if port == 0 {
		port = 587
	}
	host := strings.TrimSpace(m.SMTPHost)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	from, err := mail.ParseAddress(strings.TrimSpace(m.SMTPFrom))
	if err != nil {
		return fmt.Errorf("mail: smtp_from %q: %w", m.SMTPFrom, err)
	}
	name := m.SMTPName
	if name == "" {
		name = "supavise"
	}

	dl := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dl.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if port == 465 {
		conn = tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return err
			}
		}
	}
	if m.SMTPUser != "" {
		if err := c.Auth(smtp.PlainAuth("", m.SMTPUser, m.SMTPPass, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, composeMail((&mail.Address{Name: name, Address: from.Address}).String(), to, subject, text, time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// composeMail builds a plain-text message. Header values lose their line breaks, so nothing
// in an event (a project name, an error) can add a header.
func composeMail(from string, to []string, subject, text string, now time.Time) string {
	oneLine := func(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", oneLine(from))
	fmt.Fprintf(&b, "To: %s\r\n", oneLine(strings.Join(to, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", oneLine(subject)))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.String()
}

func (n *Notifier) mailText(ev Event) (subject, text string) {
	title := ev.Title
	if ev.Resolved {
		title += " (resolved)"
	}
	subject = fmt.Sprintf("[Supavise] %s: %s: %s", ev.severity(), n.node, title)
	var b strings.Builder
	b.WriteString(title + "\n")
	if ev.Detail != "" {
		b.WriteString("\n" + ev.Detail + "\n")
	}
	fmt.Fprintf(&b, "\nNode: %s\nKind: %s\nSeverity: %s\n", n.node, ev.Kind, ev.severity())
	if ev.Ref != "" {
		fmt.Fprintf(&b, "Project: %s\n", ev.Ref)
	}
	fmt.Fprintf(&b, "Time: %s\n", ev.Time.UTC().Format(time.RFC3339))
	return subject, b.String()
}

// Result is the outcome of one delivery.
type Result struct {
	// Destination names the endpoint (a webhook without its path, or "email to <list>").
	Destination string
	Err         error
}

// deliver sends ev to every destination at once and returns one Result each.
func (n *Notifier) deliver(ctx context.Context, ev Event) []Result {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	type job struct {
		name string
		run  func() error
	}
	var jobs []job
	for _, w := range n.cfg.Webhooks {
		jobs = append(jobs, job{redactURL(w.URL), func() error { return n.postWebhook(ctx, w, ev) }})
	}
	if to := n.cfg.Recipients(); len(to) > 0 {
		subject, text := n.mailText(ev)
		jobs = append(jobs, job{"email to " + strings.Join(to, ", "), func() error { return n.sendMail(ctx, to, subject, text) }})
	}
	res := make([]Result, len(jobs))
	done := make(chan struct{}, len(jobs))
	for i, j := range jobs {
		go func() {
			res[i] = Result{Destination: j.name, Err: j.run()}
			done <- struct{}{}
		}()
	}
	for range jobs {
		<-done
	}
	return res
}

var errNothingConfigured = errors.New("no alert destination is configured: add [[alerts.webhooks]] or [alerts] email_to to config.toml")
