package cluster

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"time"

	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/registry"
)

// Renewer replaces the node's certificate when RenewBefore of its life is left. A follower asks the
// leader (POST /peer/v1/certs/renew); the leader signs its own. The new certificate is used only
// after the node's own copy of the registry shows its serial, and a short grace after that, because
// peers admit a certificate by the serial in their copy and a copy can lag: switching at once would
// have some peers refuse the node for a moment.
type Renewer struct {
	Store     *Store
	Self      func() registry.Node
	Leader    func() (string, bool)
	IsLeader  func() bool
	RPC       mesh.RPC
	Authority *Authority
	Log       *slog.Logger
	Now       func() time.Time
	// Every is how often the certificate is looked at; zero is six hours. Grace is the wait after the
	// serial shows; zero is ten seconds. Wait bounds the wait for the serial; zero is two minutes.
	Every, Grace, Wait time.Duration
}

func (r *Renewer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run checks the certificate until ctx ends.
func (r *Renewer) Run(ctx context.Context) error {
	every := r.Every
	if every <= 0 {
		every = 6 * time.Hour
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if _, err := r.Once(ctx); err != nil && r.Log != nil {
			r.Log.Warn("certificate renewal failed; it is tried again later", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// Once renews the certificate if it is due, and reports whether it did.
func (r *Renewer) Once(ctx context.Context) (bool, error) {
	cur := r.Store.Creds()
	if cur == nil {
		return false, ErrNoIdentity
	}
	if cur.NotAfter.Sub(r.now()) > RenewBefore {
		return false, nil
	}
	key, ok := cur.Cert.PrivateKey.(ed25519.PrivateKey)
	if !ok {
		return false, fmt.Errorf("cluster: the node key is not an Ed25519 key")
	}
	csr, err := NewCSR(key, cur.NodeID)
	if err != nil {
		return false, err
	}
	var resp *peerapi.CertRenewResponse
	if r.IsLeader() {
		resp, err = r.Authority.Renew(ctx, cur.NodeID, peerapi.CertRenewRequest{CSR: csr})
	} else {
		lead, ok := r.Leader()
		if !ok {
			return false, mesh.ErrNoSession
		}
		resp = &peerapi.CertRenewResponse{}
		err = r.RPC.Call(ctx, lead, "POST", peerapi.PathCertsRenew, peerapi.CertRenewRequest{CSR: csr}, resp)
	}
	if err != nil {
		return false, err
	}
	der, err := decodePEM(resp.Cert, "CERTIFICATE")
	if err != nil {
		return false, err
	}
	wait, grace := r.Wait, r.Grace
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	if grace <= 0 {
		grace = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for r.Self().CertSerial != resp.Serial {
		if !time.Now().Before(deadline) {
			return false, fmt.Errorf("cluster: the registry copy here does not show the new certificate serial after %s", wait)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(grace):
	}
	if err := r.Store.Replace(der); err != nil {
		// The certificate is in use in memory when only the file write failed.
		if r.Store.Creds().Serial == resp.Serial {
			if r.Log != nil {
				r.Log.Warn("certificate renewed", "serial", resp.Serial, "warning", err.Error())
			}
			return true, nil
		}
		return false, err
	}
	if r.Log != nil {
		r.Log.Info("certificate renewed", "serial", resp.Serial, "not_after", resp.NotAfter.Format(time.RFC3339))
	}
	return true, nil
}
