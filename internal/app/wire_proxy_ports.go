package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/failover"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
)

// certificatesCheck is the preflight check of a server move that the proxy owns (design 2.10.2): the
// certificates this node mirrored are the ones the leader holds, so that the node that takes over
// serves every name at once. The leader's certificates are fetched the way the mirror fetches them and
// compared with the files on disk, byte for byte (certificates only: the account and the keys follow
// the same fetch).
//
// It speaks for this node only, which is the node that takes over, and only while the node mirrors. A
// leader that does not answer is the usual case of a failover: the node serves what it last mirrored,
// and the check says so and passes. A difference does not block the move: the mirror catches up within a
// minute, and a node that leads manages its certificates and issues what is missing; it is shown so
// that the operator can wait.
func certificatesCheck(src proxy.CertSource, dir string, role *proxy.CertRole, self func() string) failover.ExtraChecks {
	const name = "certificates mirrored"
	return func(ctx context.Context, to registry.Node) []failover.Check {
		if to.ID != self() || role.Managing() {
			return nil
		}
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		snap, err := src.Certs(fctx, "")
		if err != nil {
			return []failover.Check{{Name: name, OK: true, Detail: "the leader does not answer; " + to.Name + " serves the certificates it last mirrored"}}
		}
		differ, total := 0, 0
		var first string
		for _, f := range snap.Files {
			if !strings.HasSuffix(f.Path, ".crt") {
				continue
			}
			total++
			local, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
			if err != nil || !bytes.Equal(local, f.Data) {
				differ++
				if first == "" {
					first = f.Path
				}
			}
		}
		if differ == 0 {
			return []failover.Check{{Name: name, OK: true, Detail: fmt.Sprintf("%d certificate(s) match the leader's", total)}}
		}
		return []failover.Check{{Name: name, Detail: fmt.Sprintf("%d of %d certificate(s) differ from the leader's (%s): the mirror catches up within a minute", differ, total, first)}}
	}
}
