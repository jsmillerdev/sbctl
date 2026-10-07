package branching

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// validNotifyURL accepts an absolute http or https URL. Where it points is checked again
// when the call is made, at connect time, so that DNS cannot change the answer in between.
func validNotifyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalid("notify_url %q must be an absolute http or https URL", raw)
	}
	return nil
}

// newNotifyClient returns the client for notify_url calls. Unless allowPrivate is set it
// refuses to connect to loopback, private, link-local and unspecified addresses, which would
// make the daemon a way into the instance metadata service and the node's own admin ports.
func newNotifyClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			ip = ip.Unmap()
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("branching: notify_url resolves to %s, a private address ([branching] allow_private_notify_urls)", ip)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DialContext: d.DialContext, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// notify tells the branch's notify_url how an operation ended. Failures are logged and
// dropped: a webhook must not fail a merge.
func (s *Service) notify(ctx context.Context, ref, op, status, detail string) {
	p, err := s.reg.GetProject(ctx, ref)
	if err != nil || p.Branch == nil || p.Branch.NotifyURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"branch_id": p.Branch.ID, "branch_name": p.Branch.Name, "project_ref": p.Ref, "parent_project_ref": p.Branch.ParentRef,
		"operation": op, "status": status, "message": detail, "project_status": p.Status,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Branch.NotifyURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sbctl-branching")
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("branch notification failed", "ref", ref, "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.log.Warn("branch notification refused", "ref", ref, "status", resp.StatusCode)
	}
}
