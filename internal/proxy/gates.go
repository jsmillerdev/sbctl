package proxy

import (
	"net/http"
	"sync"
	"time"

	"github.com/supavise/supavise/internal/failover/fenced"
	"github.com/supavise/supavise/internal/storagemigrate/hold"
)

// Two conditions turn requests away before they are routed: the node is fenced (it was replaced as
// leader and starts no primary, design 2.10.8), and Storage's writes are held while `supavise storage
// migrate` switches its backend (design 2.11). Both are small files that another process writes, so the
// proxy reads them at most once a second.

// gateTTL is how long the answer of a gate is reused. A fence or the start or end of a Storage switch
// takes effect for requests within this time.
const gateTTL = time.Second

// gate caches what read returned for gateTTL.
type gate[T any] struct {
	mu   sync.Mutex
	at   time.Time
	have bool
	v    T
}

func (g *gate[T]) get(now time.Time, read func() T) T {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.have || now.Before(g.at) || now.Sub(g.at) >= gateTTL {
		g.v, g.at, g.have = read(), now, true
	}
	return g.v
}

// clock is the time the gates use; tests replace it.
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// fencedState is what the node record says.
type fencedState struct {
	fenced bool
	reason string
}

// fencedNow reports whether this node is fenced and why. It is fenced when it has a node record
// (internal/failover/fenced: written by a cooperative fence from the new leader, or by the node itself when
// it finds a higher epoch) or when the cluster says its role is fenced (Cluster.Fenced). A record that
// cannot be read counts as one, as it does for the plane that starts the primaries.
func (s *Server) fencedNow() (reason string, ok bool) {
	var st fencedState
	if s.cfg.StateDir != "" {
		st = s.fencedGate.get(s.clock(), func() fencedState {
			rec, err := fenced.Node(s.cfg.Paths())
			switch {
			case err != nil:
				return fencedState{fenced: true, reason: err.Error()}
			case rec != nil:
				return fencedState{fenced: true, reason: rec.Reason}
			}
			return fencedState{}
		})
	}
	if !st.fenced && s.cluster.fenced() {
		return "", true
	}
	return st.reason, st.fenced
}

// serveFenced answers every request of a fenced node: 503 with the reason, as the daemon of a node
// that boots fenced does. A browser gets the CORS headers, so that a client can read the message.
func (s *Server) serveFenced(w http.ResponseWriter, r *http.Request, reason string) {
	setCORS(w.Header(), r)
	if isPreflight(r) {
		writePreflight(w, r)
		return
	}
	if reason == "" {
		reason = "another node leads the cluster now"
	}
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusServiceUnavailable, "This Supavise node is fenced: "+reason)
}

// storageHeld reports whether Storage's writes are held by a migration at this moment.
func (s *Server) storageHeld() bool {
	if s.cfg.StateDir == "" {
		return false
	}
	return s.holdGate.get(s.clock(), func() bool { return hold.Held(s.cfg.Paths(), s.clock()) })
}

// changesData reports whether a request with this method may change data: everything but GET, HEAD
// and OPTIONS.
func changesData(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

const msgStorageHeld = "Storage is switching to another backend; writes are paused, retry in a few seconds"

// s3UnavailableBody is the document an S3 client parses from a 503 (it retries on it).
const s3UnavailableBody = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
	`<Error><Code>ServiceUnavailable</Code><Message>` + msgStorageHeld + `</Message></Error>`

// refuseStorageWrite answers a write on a Storage route while the writes are held: 503 and
// Retry-After: 5, in S3's XML on the S3 route and JSON elsewhere.
func refuseStorageWrite(w http.ResponseWriter, rt *route) {
	w.Header().Set("Retry-After", "5")
	if rt.keys == keyS3 {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(s3UnavailableBody))
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, msgStorageHeld)
}
