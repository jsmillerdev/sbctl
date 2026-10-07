package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// serve sends a request straight to the proxy's handler, with the headers exactly as given
// (duplicates, odd casing and line breaks that no HTTP client would send included), and returns
// the status, content type and body of the answer.
func (h *harness) serve(method, target string, hd http.Header) (int, string, string) {
	h.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = h.host(h.ref)
	req.Header = hd.Clone()
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
}

func (h *harness) upstreamHits() int {
	n := 0
	for _, u := range h.ups {
		n += u.count()
	}
	return n
}

func percentEncodeAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

// wireRoutes are routes of the project host, one per service and access rule.
var wireRoutes = []struct{ name, method, target string }{
	{"rest-v1", "GET", "/rest/v1/t"},
	{"rest-v1-openapi", "GET", "/rest/v1/"},
	{"graphql-v1", "POST", "/graphql/v1"},
	{"auth-v1", "GET", "/auth/v1/user"},
	{"auth-v1-admin", "GET", "/auth/v1/admin/users"},
	{"auth-v1-verify", "GET", "/auth/v1/verify"},
	{"storage-v1", "GET", "/storage/v1/bucket"},
	{"storage-v1-public", "GET", "/storage/v1/object/public/b/o"},
	{"storage-v1-s3", "GET", "/storage/v1/s3/b/o"},
	{"functions-v1", "GET", "/functions/v1/f"},
	{"realtime-v1-api", "GET", "/realtime/v1/api/ping"},
	{"realtime-v1-longpoll", "GET", "/realtime/v1/longpoll"},
	{"realtime-v1-longpoll-post", "POST", "/realtime/v1/longpoll"},
}

// A placement puts a key somewhere in a request: header values by name (all values), and a raw
// query string. pub is a valid publishable key, to show the guard does not depend on a bad key
// being the only credential.
type placement struct {
	name  string
	build func(pub, key string) (http.Header, string)
}

func wirePlacements() []placement {
	one := func(name, value string) func(pub, key string) (http.Header, string) {
		return func(pub, key string) (http.Header, string) {
			return http.Header{name: {strings.ReplaceAll(value, "KEY", key)}, "Apikey": {pub}}, ""
		}
	}
	query := func(f func(pub, key string) string) func(pub, key string) (http.Header, string) {
		return func(pub, key string) (http.Header, string) {
			return http.Header{"Apikey": {pub}}, f(pub, key)
		}
	}
	return []placement{
		{"bearer", one("Authorization", "Bearer KEY")},
		{"bare token", one("Authorization", "KEY")},
		{"bearer with tab", one("Authorization", "Bearer\tKEY")},
		{"bearer with newline", one("Authorization", "Bearer\nKEY")},
		{"bearer with obs-fold", one("Authorization", "Bearer\r\n KEY")},
		{"bearer with spaces around", one("Authorization", "Bearer   KEY   ")},
		{"mixed-case scheme", one("Authorization", "bEaReR KEY")},
		{"upper-case scheme", one("Authorization", "BEARER KEY")},
		{"another scheme", one("Authorization", "Token KEY")},
		{"basic-style prefix", one("Authorization", "Basic KEY:")},
		{"apikey header", func(pub, key string) (http.Header, string) { return http.Header{"Apikey": {key}}, "" }},
		{"duplicate authorization, legacy last", func(pub, key string) (http.Header, string) {
			return http.Header{"Authorization": {"Bearer " + pub, "Bearer " + key}, "Apikey": {pub}}, ""
		}},
		{"duplicate authorization, legacy first", func(pub, key string) (http.Header, string) {
			return http.Header{"Authorization": {"Bearer " + key, "Bearer " + pub}, "Apikey": {pub}}, ""
		}},
		{"duplicate apikey", func(pub, key string) (http.Header, string) {
			return http.Header{"Apikey": {pub, key}}, ""
		}},
		{"unrelated header", one("X-Custom-Header", "KEY")},
		{"cookie", one("Cookie", "a=b; sb=KEY; c=d")},
		{"websocket protocol", one("Sec-Websocket-Protocol", "KEY")},
		{"referer", one("Referer", "https://example.test/?k=KEY")},
		{"s3 session token header", one("X-Amz-Security-Token", "KEY")},
		{"sb-api-key header", one("Sb-Api-Key", "KEY")},
		{"x-api-key header", one("X-Api-Key", "KEY")},
		{"header with a lower-case name", func(pub, key string) (http.Header, string) {
			return http.Header{"x-lowercase-header": {key}, "Apikey": {pub}}, ""
		}},
		{"header percent-encoded", one("X-Custom-Header", "KEY")},
		{"query value", query(func(_, key string) string { return "x=" + key })},
		{"query after a valid apikey", query(func(pub, key string) string { return "apikey=" + pub + "&x=" + key })},
		{"query apikey", query(func(_, key string) string { return "apikey=" + key })},
		{"query token", query(func(_, key string) string { return "token=" + key })},
		{"query name", query(func(_, key string) string { return key + "=1" })},
		{"query without name", query(func(_, key string) string { return key })},
		{"query dot encoded", query(func(_, key string) string { return "x=" + strings.ReplaceAll(key, ".", "%2E") })},
		{"query fully encoded", query(func(_, key string) string { return "x=" + percentEncodeAll(key) })},
		{"query after a malformed escape", query(func(_, key string) string { return "bad=%zz%&x=" + key })},
		{"query url-escaped", query(func(_, key string) string { return "x=" + url.QueryEscape(key) })},
	}
}

func wireHarness(t *testing.T, disabled bool) *harness {
	t.Helper()
	h := newHarness(t, func(o *Options) { o.FunctionsEnabled = true })
	if disabled {
		k := *h.k
		k.LegacyDisabled = true
		h.keys.set(h.ref, &k)
		h.k = &k
		h.flipLegacy(t, true)
	}
	return h
}

// With the legacy keys disabled, a request that carries a legacy key anywhere in its headers or
// query string is refused on every route of the project host, before the route reads it, and
// never reaches an upstream.
func TestLegacyKeyRefusedAnywhereOnTheWire(t *testing.T) {
	h := wireHarness(t, true)
	keys := map[string]string{"anon": h.k.AnonKey, "service_role": h.k.ServiceRoleKey}
	// The signing input alone, without the signature, also gives a key away.
	for n, key := range map[string]string{"anon": h.k.AnonKey, "service_role": h.k.ServiceRoleKey} {
		keys[n+" without signature"], _ = signingInput(key)
		keys[n+" with a cut signature"] = key[:len(key)-20]
	}
	for n, v := range reencodings(t, h.k.ServiceRoleKey) {
		keys["service_role "+n] = v
	}
	before := h.upstreamHits()
	var checked int
	for _, pl := range wirePlacements() {
		for kname, key := range keys {
			hd, q := pl.build(h.k.PublishableKey, key)
			if pl.name == "header percent-encoded" {
				hd.Set("X-Custom-Header", percentEncodeAll(key))
			}
			for _, rt := range wireRoutes {
				target := rt.target
				if q != "" {
					target += "?" + q
				}
				code, ctype, body := h.serve(rt.method, target, hd)
				checked++
				if rt.name == "storage-v1-s3" {
					if code != 403 || !strings.HasPrefix(ctype, "application/xml") || !strings.Contains(body, "<Code>AccessDenied</Code>") {
						t.Errorf("%s / %s / %s: %d %q %q, want 403 AccessDenied XML", rt.name, pl.name, kname, code, ctype, body)
					}
					continue
				}
				var msg struct{ Message string }
				if code != 401 || !strings.HasPrefix(ctype, "application/json") || json.Unmarshal([]byte(body), &msg) != nil || msg.Message != msgInvalidKey {
					t.Errorf("%s / %s / %s: %d %q %q, want 401 JSON %q", rt.name, pl.name, kname, code, ctype, body, msgInvalidKey)
				}
			}
		}
	}
	if got := h.upstreamHits(); got != before {
		t.Errorf("%d requests with a legacy key reached an upstream (of %d)", got-before, checked)
	}
}

// Legitimate traffic is untouched: opaque keys, user sessions signed with the same secret, a
// token with the legacy header and another payload, S3 access keys and sessions.
func TestLegacyGuardLeavesLegitimateTrafficAlone(t *testing.T) {
	h := wireHarness(t, true)
	user := signJWT(t, h.k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u1", "ref": h.ref, "exp": time.Now().Add(time.Hour).Unix()})
	// Same header as the legacy keys (HS256, same encoder), another payload.
	if strings.SplitN(user, ".", 2)[0] != strings.SplitN(h.k.AnonKey, ".", 2)[0] {
		t.Logf("the test JWT header differs from the legacy keys' header; the payload check is what is tested")
	}
	anonLike := signJWT(t, h.k.JWTSecret, jwt.MapClaims{"role": "anon", "iss": "supabase", "ref": h.ref, "exp": time.Now().Add(time.Hour).Unix()})
	sigv4 := "AWS4-HMAC-SHA256 Credential=" + h.ref + "/20250101/local/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=0123456789abcdef"
	pub, sec := h.k.PublishableKey, h.k.SecretKey
	for name, hd := range map[string]http.Header{
		"publishable apikey":               {"Apikey": {pub}},
		"secret apikey":                    {"Apikey": {sec}},
		"secret as bearer":                 {"Authorization": {"Bearer " + sec}},
		"user session as bearer":           {"Apikey": {pub}, "Authorization": {"Bearer " + user}},
		"user session, tab after scheme":   {"Apikey": {pub}, "Authorization": {"Bearer\t" + user}},
		"anon-like token, other payload":   {"Apikey": {pub}, "Authorization": {"Bearer " + anonLike}},
		"two bearers, none legacy":         {"Apikey": {pub}, "Authorization": {"Bearer " + user, "Bearer " + anonLike}},
		"cookie and referer":               {"Apikey": {pub}, "Cookie": {"a=b"}, "Referer": {"https://app.example/"}},
		"s3 sigv4 with a user session":     {"Authorization": {sigv4}, "X-Amz-Security-Token": {user}},
		"s3 sigv4 with access keys":        {"Authorization": {sigv4}},
		"s3 sigv4 with the secret session": {"Authorization": {sigv4}, "X-Amz-Security-Token": {sec}},
	} {
		for _, rt := range wireRoutes {
			// The S3 rules about POST uploads and the route's own access rules are not the point.
			code, _, body := h.serve(rt.method, rt.target+"?filter=eq.1&sig="+percentEncodeAll(user), hd)
			if code == 401 && strings.Contains(body, msgInvalidKey) {
				t.Errorf("%s on %s: refused as a legacy key: %d %q", name, rt.name, code, body)
			}
			if code == 403 && strings.Contains(body, "AccessDenied") {
				t.Errorf("%s on %s: refused on S3: %d %q", name, rt.name, code, body)
			}
		}
	}
	// A request that is fine is also forwarded.
	if code, _, _ := h.serve("GET", "/rest/v1/t", http.Header{"Apikey": {pub}, "Authorization": {"Bearer " + user}}); code != 200 {
		t.Errorf("user session with a publishable key on rest-v1: %d", code)
	}
	if code, _, _ := h.serve("GET", "/storage/v1/bucket", http.Header{"Authorization": {"Bearer " + user}}); code != 200 {
		t.Errorf("user session on storage-v1: %d", code)
	}
	if code, _, _ := h.serve("GET", "/storage/v1/s3/b/o", http.Header{"Authorization": {sigv4}, "X-Amz-Security-Token": {user}}); code != 200 {
		t.Errorf("S3 request with a user session token: %d", code)
	}
	if code, _, _ := h.serve("GET", "/functions/v1/f", http.Header{"Authorization": {"Bearer " + user}}); code != 200 {
		t.Errorf("user session on functions-v1: %d", code)
	}
}

// While the legacy keys are enabled the guard does nothing.
func TestLegacyGuardOffWhileEnabled(t *testing.T) {
	h := wireHarness(t, false)
	for _, pl := range wirePlacements() {
		hd, q := pl.build(h.k.PublishableKey, h.k.ServiceRoleKey)
		target := "/storage/v1/bucket"
		if q != "" {
			target += "?" + q
		}
		code, _, body := h.serve("GET", target, hd)
		if code == 401 && strings.Contains(body, msgInvalidKey) {
			t.Errorf("%s: refused while the legacy keys are enabled: %d %q", pl.name, code, body)
		}
	}
}

func TestLegacyWireNeedles(t *testing.T) {
	k := testKeys(t, testRef)
	if legacyWireNeedles(k) != nil {
		t.Error("needles while the legacy keys are enabled")
	}
	k.LegacyDisabled = true
	got := legacyWireNeedles(k)
	if len(got) != 2 {
		t.Fatalf("needles: %q", got)
	}
	for i, key := range []string{k.AnonKey, k.ServiceRoleKey} {
		if in, _ := signingInput(key); got[i] != in || strings.HasSuffix(got[i], ".") || !strings.Contains(string(legacyNeedles(k)[i]), got[i]) {
			t.Errorf("needle %d: %q", i, got[i])
		}
	}
	if !carriesLegacyKey(k, http.Header{"X": {"a" + k.AnonKey + "b"}}, "") || carriesLegacyKey(k, http.Header{"X": {"eyJhbGciOiJIUzI1NiJ9.e30.x"}}, "a=b") {
		t.Error("carriesLegacyKey")
	}
	if percentDecode("a%2Eb%zz%4") != "a.b%zz%4" || percentDecode("%41%61") != "Aa" {
		t.Errorf("percentDecode: %q %q", percentDecode("a%2Eb%zz%4"), percentDecode("%41%61"))
	}
	if got := queryValues("a=1&token=x%2Ey&Token=z&token=%zz&token", "token"); len(got) != 2 || got[0] != "x.y" || got[1] != "%zz" {
		t.Errorf("queryValues: %q", got)
	}
}

// ---- Realtime long-poll sessions ----

// serveFakeLongPoll makes Realtime behave like Phoenix's long-poll transport: a GET without a
// token opens a session (HTTP 410 and a token in the body), anything with a token resumes it.
func serveFakeLongPoll(h *harness) {
	var n int32
	rt := h.ups[svcRealtime]
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Query().Get("token") == "" {
			i := atomic.AddInt32(&n, 1)
			w.WriteHeader(http.StatusGone)
			fmt.Fprintf(w, `{"token":"SFMyNTY.session-%d.sig_%d","messages":[]}`, i, i)
			return
		}
		fmt.Fprint(w, `{"status":200,"messages":[]}`)
	}
}

func (h *harness) openSession(t *testing.T, apikey string) string {
	t.Helper()
	resp, body := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?vsn=2.0.0&apikey="+apikey)
	var v struct{ Token string }
	if resp.StatusCode != http.StatusGone || json.Unmarshal([]byte(body), &v) != nil || v.Token == "" {
		t.Fatalf("opening a session: %d %q", resp.StatusCode, body)
	}
	return v.Token
}

func (h *harness) pollSession(method, token string) (int, string) {
	h.t.Helper()
	resp, body := h.reqBody(method, h.host(h.ref), "/realtime/v1/longpoll?vsn=2.0.0&apikey="+h.k.PublishableKey+"&token="+token, `[]`)
	return resp.StatusCode, body
}

func (h *harness) realtimeHits() int { return h.ups[svcRealtime].count() }

func TestRealtimeLongPollSessionOpenedBeforeSwitchIsRevoked(t *testing.T) {
	h := newHarness(t) // legacy keys enabled
	serveFakeLongPoll(h)
	a := h.openSession(t, h.k.ServiceRoleKey) // connected with a legacy key
	b := h.openSession(t, h.k.PublishableKey)
	if code, _ := h.pollSession("GET", a); code != 200 {
		t.Fatalf("poll before the switch: %d", code)
	}
	if code, _ := h.pollSession("POST", a); code != 200 {
		t.Fatalf("push before the switch: %d", code)
	}
	eventually(t, "both sessions tracked", func() bool {
		h.srv.lp.mu.Lock()
		defer h.srv.lp.mu.Unlock()
		return len(h.srv.lp.m[h.ref].live) == 2
	})

	h.flipLegacy(t, true)
	eventually(t, "sessions revoked", func() bool { return len(h.srv.lp.refs(h.ref)) == 0 })

	hits := h.realtimeHits()
	for _, tok := range []string{a, b, strings.ReplaceAll(a, ".", "%2E"), "other&token=" + a} {
		for _, method := range []string{"GET", "POST"} {
			code, body := h.pollSession(method, tok)
			if code != http.StatusGone || !strings.Contains(body, `"status":410`) || strings.Contains(body, `"token"`) {
				t.Errorf("%s with revoked token %q: %d %q, want 410 without a token", method, tok, code, body)
			}
		}
	}
	// A duplicate token parameter does not hide the revoked one.
	if resp, _ := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?apikey="+h.k.PublishableKey+"&token=fresh&token="+a); resp.StatusCode != http.StatusGone {
		t.Errorf("duplicate token parameter: %d", resp.StatusCode)
	}
	if got := h.realtimeHits(); got != hits {
		t.Errorf("%d resumed requests with a revoked token reached Realtime", got-hits)
	}

	// The client starts a new session through the guarded path: a legacy key is refused, the
	// publishable key opens a session that keeps working, and its token is not revoked later.
	if resp, _ := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?apikey="+h.k.ServiceRoleKey); resp.StatusCode != 401 {
		t.Errorf("a new session with a legacy key: %d", resp.StatusCode)
	}
	c := h.openSession(t, h.k.PublishableKey)
	if code, _ := h.pollSession("GET", c); code != 200 {
		t.Errorf("poll of a session opened after the switch: %d", code)
	}
	h.flipLegacy(t, true) // another notification
	time.Sleep(50 * time.Millisecond)
	if code, _ := h.pollSession("GET", c); code != 200 {
		t.Errorf("session opened after the switch revoked by a later notification: %d", code)
	}
	if code, _ := h.pollSession("GET", a); code != http.StatusGone {
		t.Errorf("revoked token after another notification: %d", code)
	}
}

// Sessions stay valid while the keys change but legacy stays enabled.
func TestRealtimeLongPollSessionStaysWhileLegacyKeysAreEnabled(t *testing.T) {
	h := newHarness(t)
	serveFakeLongPoll(h)
	a := h.openSession(t, h.k.ServiceRoleKey)
	calls := h.keys.callCount(h.ref)
	h.flipLegacy(t, false) // a rotation, say
	eventually(t, "keys looked up again", func() bool { return h.keys.callCount(h.ref) > calls })
	time.Sleep(50 * time.Millisecond)
	if code, _ := h.pollSession("GET", a); code != 200 {
		t.Errorf("poll after a key change with the legacy keys enabled: %d", code)
	}
	if got := len(h.srv.lp.refs(h.ref)); got != 1 {
		t.Errorf("tracked projects: %d", got)
	}
}

// A session whose opening raced the switch (keys read before it, answer after it) is caught by
// the check that follows its registration.
func TestRealtimeLongPollSessionOpenedDuringSwitchIsRevoked(t *testing.T) {
	h := newHarness(t)
	serveFakeLongPoll(h)
	// Sync's first full reload drops every cached key; let it happen before the stale entry goes in.
	eventually(t, "the first reload", func() bool {
		h.srv.table.mu.RLock()
		defer h.srv.table.mu.RUnlock()
		return h.srv.table.keyEpoch >= 2
	})
	k := *h.k
	k.LegacyDisabled = true
	h.keys.set(h.ref, &k)
	h.srv.table.mu.Lock()
	h.srv.table.keyCache[h.ref] = keyEntry{keys: h.k, expires: time.Now().Add(time.Hour)}
	h.srv.table.mu.Unlock()
	a := h.openSession(t, h.k.ServiceRoleKey) // authorized on the stale, enabled keys
	h.srv.table.invalidateKeys(h.ref)         // the late notification
	eventually(t, "session revoked", func() bool { return len(h.srv.lp.refs(h.ref)) == 0 })
	if code, _ := h.pollSession("GET", a); code != http.StatusGone {
		t.Errorf("session opened during the switch: %d", code)
	}
}

// A project's sessions do not affect another project's.
func TestRealtimeLongPollRevocationIsPerProject(t *testing.T) {
	h := newHarness(t)
	serveFakeLongPoll(h)
	h.srv.lp.add("otherproject00000000", "SFMyNTY.x.y")
	a := h.openSession(t, h.k.PublishableKey)
	h.flipLegacy(t, true)
	eventually(t, "revoked", func() bool { return len(h.srv.lp.refs(h.ref)) == 0 })
	if code, _ := h.pollSession("GET", a); code != http.StatusGone {
		t.Errorf("own session: %d", code)
	}
	if got := h.srv.lp.refs("otherproject00000000"); len(got) != 1 {
		t.Errorf("another project's session was touched: %v", got)
	}
}

// The answer to a new session is passed on byte for byte, large or odd answers included.
func TestRealtimeLongPollAnswerPassesThrough(t *testing.T) {
	h := newHarness(t)
	rt := h.ups[svcRealtime]
	big := `{"token":"SFMyNTY.big.sig","messages":["` + strings.Repeat("m", lpMaxSessionRs) + `"]}`
	answers := []string{`not json`, `{"token":""}`, `{"token":5}`, big, ""}
	var i int32
	rt.mu.Lock()
	rt.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, answers[int(atomic.AddInt32(&i, 1))-1])
	}
	rt.mu.Unlock()
	for _, want := range answers {
		resp, body := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?apikey="+h.k.PublishableKey)
		if resp.StatusCode != http.StatusGone || body != want {
			t.Errorf("answer %.30q came back as %d %.30q (%d bytes)", want, resp.StatusCode, body, len(body))
		}
	}
	// Only the large answer's token could have been tracked, and it is too large to be read.
	time.Sleep(20 * time.Millisecond)
	if got := h.srv.lp.refs(""); len(got) != 0 {
		t.Errorf("tracked from an unreadable answer: %v", got)
	}
}

func TestLongPollSessionBookkeeping(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	s := &lpSessions{now: func() time.Time { return now }}
	s.add("p", "t1")
	now = now.Add(lpLiveIdle - time.Second)
	s.seen("p", []string{"t1"}) // a poll keeps it alive
	now = now.Add(lpLiveIdle - time.Second)
	s.add("p", "t2") // prunes only what is idle
	if n := s.revoke("p"); n != 2 {
		t.Errorf("revoked %d, want 2 (t1 was polled)", n)
	}
	if !s.seen("p", []string{"zzz", "t1"}) {
		t.Error("t1 not refused")
	}
	now = now.Add(lpRevokedFor + time.Second)
	if s.seen("p", []string{"t1"}) {
		t.Error("revocation never expires")
	}
	s.add("p", "idle")
	now = now.Add(lpLiveIdle + time.Second)
	s.add("p", "fresh")
	if n := s.revoke("p"); n != 1 {
		t.Errorf("an idle session was still tracked: revoked %d", n)
	}
	// The cap drops the least recently used session.
	for i := 0; i < lpMaxLive+5; i++ {
		now = now.Add(time.Millisecond)
		s.add("q", fmt.Sprint("t", i))
	}
	if n := len(s.m["q"].live); n != lpMaxLive {
		t.Errorf("tracked %d, cap %d", n, lpMaxLive)
	}
	if _, ok := s.m["q"].live["t0"]; ok {
		t.Error("the oldest session survived the cap")
	}
	if _, ok := s.m["q"].live[fmt.Sprint("t", lpMaxLive+4)]; !ok {
		t.Error("the newest session was dropped")
	}
}

// A failed key lookup during the recheck does not leave the tracked connections open for good.
func TestRealtimeRecheckRetriesAfterFailedLookup(t *testing.T) {
	h := newHarness(t)
	serveFakeLongPoll(h)
	h.srv.recheckWait = 10 * time.Millisecond
	a := h.openSession(t, h.k.ServiceRoleKey)
	k := *h.k
	k.LegacyDisabled = true
	h.keys.set(h.ref, &k)
	h.keys.setFail(errors.New("registry down"))
	h.srv.table.invalidateKeys(h.ref)
	time.Sleep(60 * time.Millisecond)
	if len(h.srv.lp.refs(h.ref)) != 1 {
		t.Fatal("the session was revoked without knowing the keys")
	}
	h.keys.setFail(nil)
	eventually(t, "the retry revoked the session", func() bool { return len(h.srv.lp.refs(h.ref)) == 0 })
	if code, _ := h.pollSession("GET", a); code != http.StatusGone {
		t.Errorf("poll after the retry: %d", code)
	}
}
