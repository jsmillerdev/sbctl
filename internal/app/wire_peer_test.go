package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/supavise/supavise/internal/cluster"
	"github.com/supavise/supavise/internal/mesh"
	"github.com/supavise/supavise/internal/mesh/peerapi"
	"github.com/supavise/supavise/internal/proxy"
	"github.com/supavise/supavise/internal/registry"
)

// clientRPC is a mesh.RPC over the one session of a client.
type clientRPC struct{ c *mesh.Client }

func (r clientRPC) Call(ctx context.Context, _, method, path string, in, out any) error {
	return r.c.Call(ctx, method, path, in, out)
}

// The seams that cross the peer server, against the real one of a leader that every hook started. The
// proxy's way to fetch the leader's certificates (proxy.MeshCerts, which tags its request with the etag
// query): the query reaches the handler, and a store the caller has comes back as ErrCertsNotModified
// instead of the whole store. The report a node sends: the leader stores it under the node of the
// certificate and hands it to the replica controller.
func TestThePeerServerAnswersTheProxyAndTheReplicaController(t *testing.T) {
	w := clusterNodeWire(t)
	t.Cleanup(w.stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.run(ctx); err != nil {
		t.Fatal(err)
	}
	g, gctx := errgroup.WithContext(ctx)
	w.start(g, gctx)

	// A second node of the cluster, with a certificate the leader's CA signed.
	reg := w.Node.Registry
	ca, err := cluster.NewCA(w.Node.Secrets.(cluster.Deriver))
	if err != nil {
		t.Fatal(err)
	}
	var n2 *registry.Node
	nodes, _ := reg.ListNodes(ctx)
	for i := range nodes {
		if nodes[i].ID != "n1" {
			n2 = &nodes[i]
		}
	}
	if n2 == nil {
		t.Fatal("the fixture has no second node")
	}
	key, _ := cluster.NewKey()
	iss, err := ca.Issue(key.Public().(ed25519.PublicKey), n2.ID, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetNodeCert(ctx, n2.ID, iss.Serial); err != nil {
		t.Fatal(err)
	}
	creds, err := mesh.NewCredentials(iss.DER, key, ca.Cert())
	if err != nil {
		t.Fatal(err)
	}
	live, _ := Get[*cluster.Live](w)
	eventually(t, "the leader to know the second node", func() bool { live.Refresh(ctx); return len(live.Nodes()) == 2 })
	admit := mesh.AdmitFromNodes(func() []registry.Node { ns, _ := reg.ListNodes(ctx); return ns })
	var c *mesh.Client
	eventually(t, "the second node to reach the leader", func() bool {
		var err error
		c, err = mesh.DialClient(ctx, w.Cfg.PeerListen(), "n1", mesh.ClientTLS(func() *mesh.Credentials { return creds }, "n1", admit, time.Now))
		return err == nil
	})
	defer c.Close()

	src := proxy.MeshCerts{RPC: clientRPC{c}, Leader: func() (string, bool) { return "n1", true }}
	snap, err := src.Certs(ctx, "")
	if err != nil {
		t.Fatalf("the first fetch: %v", err)
	}
	if len(snap.Files) != 0 {
		t.Fatalf("an empty store held %d files", len(snap.Files))
	}
	// The tag of an empty store is the hash of nothing; a caller that holds the store gets a 304.
	const emptyTag = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if _, err := src.Certs(ctx, emptyTag); !errors.Is(err, proxy.ErrCertsNotModified) {
		t.Fatalf("a fetch with the store's tag: %v, want ErrCertsNotModified", err)
	}
	if _, err := src.Certs(ctx, "another-tag"); err != nil {
		t.Fatalf("a fetch with another tag: %v", err)
	}

	// What a node reports reaches the leader's store under the node of its certificate, and a node
	// cannot report for another.
	reports, _ := Get[*cluster.Reports](w)
	err = c.Call(ctx, "POST", peerapi.PathReport, peerapi.Report{Instances: []peerapi.InstanceStatus{{Identifier: "x", Ref: "abcdefghijklmnopqrst"}}}, nil)
	if err != nil {
		t.Fatalf("a report: %v", err)
	}
	eventually(t, "the report to be stored", func() bool { _, ok := reports.Latest("n2"); return ok })
	if st, _, ok := reports.Instance("x"); !ok || st.Ref != "abcdefghijklmnopqrst" {
		t.Fatalf("instance: %+v %v", st, ok)
	}
	var re *mesh.RemoteError
	if err := c.Call(ctx, "POST", peerapi.PathReport, peerapi.Report{Node: "n3"}, nil); !errors.As(err, &re) || re.Status != 403 {
		t.Fatalf("a report in another node's name: %v", err)
	}
	cancel()
	_ = g.Wait()
}
