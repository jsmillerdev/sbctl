package failover

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// publicProbeTimeout bounds one look at the public address.
const publicProbeTimeout = 8 * time.Second

// PublicProbe returns the probe of the cluster's public address that the automatic server mode
// needs as its second signal: GET /healthz of the API host, asked the way a client asks, but
// connecting to the service address instead of whatever DNS says. A leader that no peer reaches
// may still serve its clients, and then nothing is taken over.
//
// host is the API host ("api.example.com"); ip returns the service address, "" when the cluster
// has none (the probe then fails: the signal cannot be had). With plainHTTP the probe uses port 80 and
// no TLS (a node whose [tls] mode is off). The answer is the node's verdict: 200 for healthy and
// degraded, 503 for down.
func PublicProbe(host string, ip func(ctx context.Context) string, plainHTTP bool) func(ctx context.Context) error {
	if plainHTTP {
		return probeAt(host, ip, "80", "http")
	}
	return probeAt(host, ip, "443", "https")
}

func probeAt(host string, ip func(ctx context.Context) string, port, scheme string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		addr := ip(ctx)
		if addr == "" {
			return fmt.Errorf("the cluster has no service address to probe")
		}
		ctx, cancel := context.WithTimeout(ctx, publicProbeTimeout)
		defer cancel()
		tr := &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort(addr, port))
			},
			TLSClientConfig:   &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		}
		defer tr.CloseIdleConnections()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s answered %d", host, resp.StatusCode)
		}
		return nil
	}
}
