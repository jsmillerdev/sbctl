package cluster

import (
	"context"
	"crypto/ed25519"
	"errors"
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
	// Blocked, when set, is told why the certificate cannot be renewed (the daemon cannot write the
	// cluster directory), and told nil when it can, each time the directory is looked at.
	Blocked func(err error)
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

// writable checks the cluster directory and tells Blocked what it found.
func (r *Renewer) writable() error {
	err := r.Store.Writable()
	if r.Blocked != nil {
		r.Blocked(err)
	}
	return err
}

// Run checks the certificate until ctx ends. It looks at the cluster directory when it starts, so
// that a unit that does not let the daemon write there is reported while the certificate has most of
// its life left, not on the day it has to be replaced.
func (r *Renewer) Run(ctx context.Context) error {
	every := r.Every
	if every <= 0 {
		every = 6 * time.Hour
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	until := ""
	if c := r.Store.Creds(); c != nil {
		until = c.NotAfter.Format(time.RFC3339)
	}
	if err := r.writable(); err != nil {
		if r.Log != nil {
			r.Log.Error("the node certificate cannot be renewed; the current one works until it expires", "not_after", until, "error", err)
		}
	} else if r.Log != nil {
		r.Log.Info("the node certificate is renewed before it expires", "not_after", until)
	}
	for {
		if _, err := r.Once(ctx); err != nil && r.Log != nil {
			if errors.Is(err, ErrIdentityReadOnly) {
				r.Log.Error("the node certificate is due and cannot be renewed", "error", err)
			} else {
				r.Log.Warn("certificate renewal failed; it is tried again later", "error", err)
			}
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
	// A new certificate that the node could not keep would lock it out at its next restart, so the
	// old one is left to run out in that case.
	if err := r.writable(); err != nil {
		return false, err
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
		// The leader has recorded the new serial, so the old file is no good to anyone: the new
		// certificate stays in use in memory and the operator is told the file is stale.
		if r.Store.Creds().Serial == resp.Serial {
			if r.Log != nil {
				r.Log.Error("certificate renewed but its file was not written; a restart of the daemon would lock the node out", "serial", resp.Serial, "error", err.Error())
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
