package proxy

import (
	"context"
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
	{"realtime-v1-ws", "GET", "/realtime/v1/websocket"},
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

// wsHeaders turns hd into the headers of a WebSocket handshake.
func wsHeaders(hd http.Header) http.Header {
	hd = hd.Clone()
	hd.Set("Connection", "Upgrade")
	hd.Set("Upgrade", "websocket")
	return hd
}

// With the legacy keys disabled, a request that carries a legacy key anywhere in its headers or
// query string is refused on every route of the project host, before the route reads it, and
// never reaches an upstream. The Realtime long-poll routes refuse it as every long-poll request
// is refused (403), a WebSocket handshake with the key (401).
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
				rhd := hd
				if rt.name == "realtime-v1-ws" {
					rhd = wsHeaders(hd)
				}
				code, ctype, body := h.serve(rt.method, target, rhd)
				checked++
				if rt.name == "storage-v1-s3" {
					if code != 403 || !strings.HasPrefix(ctype, "application/xml") || !strings.Contains(body, "<Code>AccessDenied</Code>") {
						t.Errorf("%s / %s / %s: %d %q %q, want 403 AccessDenied XML", rt.name, pl.name, kname, code, ctype, body)
					}
					continue
				}
				wantCode, wantMsg := 401, msgInvalidKey
				if strings.HasPrefix(rt.name, "realtime-v1-longpoll") {
					wantCode, wantMsg = 403, msgRealtimeLongPollOff
				}
				var msg struct{ Message string }
				if code != wantCode || !strings.HasPrefix(ctype, "application/json") || json.Unmarshal([]byte(body), &msg) != nil || msg.Message != wantMsg {
					t.Errorf("%s / %s / %s: %d %q %q, want %d JSON %q", rt.name, pl.name, kname, code, ctype, body, wantCode, wantMsg)
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
			if rt.name == "realtime-v1-ws" {
				continue // needs a real connection; the socket tests cover it
			}
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
}

// ---- Realtime is WebSocket-only while the legacy keys are disabled ----

// serveFakeLongPoll makes Realtime behave like Phoenix's long-poll transport: a GET without a
// token opens a session (HTTP 410 and a token in the body), anything with a token resumes it.
func serveFakeLongPoll(h *harness) {
	var n int32
	rt := h.ups[svcRealtime]
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Query().Get("token") == "" && r.Header.Get("X-Phoenix-Longpoll-Token") == "" {
			i := atomic.AddInt32(&n, 1)
			w.WriteHeader(http.StatusGone)
			fmt.Fprintf(w, `{"token":"SFMyNTY.session-%d.sig_%d","messages":[]}`, i, i)
			return
		}
		fmt.Fprint(w, `{"status":200,"messages":[]}`)
	}
}

func (h *harness) realtimeHits() int { return h.ups[svcRealtime].count() }

// A long-poll request in one of the forms a client or an attacker can send.
type lpForm struct {
	name, method, target, body string
	headers                    []string
}

// longPollForms are the long-poll requests that must be refused while the keys are disabled and
// that work while they are enabled. stale is the token of a session opened while they were enabled.
func longPollForms(h *harness, stale string) []lpForm {
	pub, svc := h.k.PublishableKey, h.k.ServiceRoleKey
	base := "/realtime/v1/longpoll?vsn=2.0.0&apikey=" + pub
	join := `{"topic":"realtime:x","event":"phx_join","payload":{"access_token":"` + pub + `"}}`
	jh := []string{"Content-Type", "application/json"}
	return []lpForm{
		{"GET, no token", "GET", base, "", nil},
		{"GET, query token", "GET", base + "&token=" + stale, "", nil},
		{"GET, query token, re-spelled name", "GET", base + "&to%6Ben=" + stale, "", nil},
		{"GET, query token, re-spelled value", "GET", base + "&token=" + strings.ReplaceAll(stale, ".", "%2E"), "", nil},
		{"GET, query token twice", "GET", base + "&token=fresh&token=" + stale, "", nil},
		{"GET, query token, other case", "GET", base + "&Token=" + stale, "", nil},
		{"GET, token header", "GET", base, "", []string{"X-Phoenix-Longpoll-Token", stale}},
		{"GET, token header and query token", "GET", base + "&token=" + stale, "", []string{"X-Phoenix-Longpoll-Token", stale}},
		{"GET, stale token", "GET", base + "&token=SFMyNTY.long-gone.sig", "", nil},
		{"GET, empty token", "GET", base + "&token=", "", nil},
		{"GET, publishable key in a header", "GET", "/realtime/v1/longpoll?vsn=2.0.0", "", []string{"Apikey", pub}},
		{"GET, no key at all", "GET", "/realtime/v1/longpoll", "", nil},
		{"GET, trailing slash", "GET", "/realtime/v1/longpoll/?apikey=" + pub, "", nil},
		{"GET, dot segments", "GET", "/realtime/v1/x/../longpoll?apikey=" + pub, "", nil},
		{"POST, token, clean body", "POST", base + "&token=" + stale, join, jh},
		{"POST, no token, clean body", "POST", base, join, jh},
		{"POST, no body", "POST", base + "&token=" + stale, "", nil},
		{"POST, legacy key in the body", "POST", base + "&token=" + stale, `{"payload":{"access_token":"` + svc + `"}}`, jh},
		{"POST, re-encoded key in the body", "POST", base, `{"payload":{"access_token":"` + reencodings(h.t, svc)["= padding"] + `"}}`, jh},
		{"POST, large body", "POST", base + "&token=" + stale, strings.Repeat("p", 300<<10), nil},
		{"PUT", "PUT", base + "&token=" + stale, join, jh},
		{"DELETE", "DELETE", base + "&token=" + stale, "", nil},
		{"HEAD", "HEAD", base, "", nil},
		{"POST with upgrade headers", "POST", base, join, append([]string{"Connection", "Upgrade", "Upgrade", "websocket"}, jh...)},
		{"GET with Upgrade only", "GET", base, "", []string{"Upgrade", "websocket"}},
		{"GET with another upgrade protocol", "GET", base, "", []string{"Connection", "Upgrade", "Upgrade", "h2c"}},
		{"handshake to longpoll", "GET", base, "", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"handshake to another subpath", "GET", "/realtime/v1/socket?apikey=" + pub, "", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"handshake to websocket subpath", "GET", "/realtime/v1/websocket/x?apikey=" + pub, "", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"handshake to websocket, trailing slash", "GET", "/realtime/v1/websocket/?apikey=" + pub, "", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"handshake to the prefix", "GET", "/realtime/v1?apikey=" + pub, "", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"handshake with a body", "GET", "/realtime/v1/websocket?apikey=" + pub, "x", []string{"Connection", "Upgrade", "Upgrade", "websocket"}},
		{"websocket path without a handshake", "GET", "/realtime/v1/websocket?apikey=" + pub, "", nil},
		{"websocket path, POST", "POST", "/realtime/v1/websocket?apikey=" + pub, join, jh},
		{"other subpath", "GET", "/realtime/v1/anything?apikey=" + pub, "", nil},
		{"the prefix", "GET", "/realtime/v1?apikey=" + pub, "", nil},
	}
}

func TestRealtimeLongPollRefusedWhileLegacyKeysDisabled(t *testing.T) {
	h := wireHarness(t, true)
	serveFakeLongPoll(h)
	hits := h.realtimeHits()
	for _, f := range longPollForms(h, "SFMyNTY.session-1.sig_1") {
		resp, body := h.reqBody(f.method, h.host(h.ref), f.target, f.body, f.headers...)
		var msg struct{ Message string }
		if f.method != "HEAD" && (json.Unmarshal([]byte(body), &msg) != nil || msg.Message != msgRealtimeLongPollOff) {
			t.Errorf("%s: body %q, want the JSON message %q", f.name, body, msgRealtimeLongPollOff)
		}
		if resp.StatusCode != http.StatusForbidden || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d %q, want 403 JSON", f.name, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	if got := h.realtimeHits(); got != hits {
		t.Errorf("%d long-poll requests reached Realtime", got-hits)
	}
	// The message is the one the decision names.
	if msgRealtimeLongPollOff != "Realtime long-polling is unavailable while legacy API keys are disabled; use WebSocket" {
		t.Errorf("message: %q", msgRealtimeLongPollOff)
	}
}

// Realtime's /api routes are not long poll and keep working with the legacy keys off.
func TestRealtimeAPIUnaffectedByLongPollRefusal(t *testing.T) {
	h := wireHarness(t, true)
	if code, _, _ := h.serve("GET", "/realtime/v1/api/ping", http.Header{"Apikey": {h.k.PublishableKey}}); code != 200 {
		t.Errorf("realtime api with the legacy keys off: %d", code)
	}
}

// While the keys are enabled every one of those forms is forwarded as before, and the sessions
// it opens work; when the keys are switched off, the same sessions die because every later
// request is refused, and a real WebSocket still works.
func TestRealtimeLongPollWorksWhileEnabledAndDiesOnSwitch(t *testing.T) {
	h := newHarness(t)
	serveFakeLongPoll(h)
	open := func(apikey string) string {
		t.Helper()
		resp, body := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?vsn=2.0.0&apikey="+apikey)
		var v struct{ Token string }
		if resp.StatusCode != http.StatusGone || json.Unmarshal([]byte(body), &v) != nil || v.Token == "" {
			t.Fatalf("opening a session: %d %q", resp.StatusCode, body)
		}
		return v.Token
	}
	a := open(h.k.ServiceRoleKey) // connected with a legacy key
	b := open(h.k.PublishableKey)
	for _, tok := range []string{a, b} {
		for _, f := range longPollForms(h, tok) {
			if !strings.HasPrefix(f.name, "GET, query token") && !strings.HasPrefix(f.name, "POST, token") && f.name != "GET, token header" {
				continue
			}
			if strings.Contains(f.name, "re-spelled name") || strings.Contains(f.name, "other case") {
				continue // the fake Realtime does not read those spellings as a token
			}
			resp, body := h.reqBody(f.method, h.host(h.ref), f.target, f.body, f.headers...)
			if resp.StatusCode != 200 {
				t.Errorf("%s while enabled: %d %q", f.name, resp.StatusCode, body)
			}
		}
	}
	before := h.realtimeHits()
	if resp, _ := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?apikey="+h.k.PublishableKey); resp.StatusCode != http.StatusGone {
		t.Errorf("a new session while enabled: %d", resp.StatusCode)
	}
	if h.realtimeHits() != before+1 {
		t.Errorf("the new session did not reach Realtime")
	}

	h.flipLegacy(t, true)
	hits := h.realtimeHits()
	for _, tok := range []string{a, b} {
		for _, f := range longPollForms(h, tok) {
			resp, _ := h.reqBody(f.method, h.host(h.ref), f.target, f.body, f.headers...)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s after the switch: %d, want 403", f.name, resp.StatusCode)
			}
		}
	}
	if got := h.realtimeHits(); got != hits {
		t.Errorf("%d long-poll requests reached Realtime after the switch", got-hits)
	}

	// Switched back on, long poll works again.
	h.flipLegacy(t, false)
	if resp, _ := h.req("GET", h.host(h.ref), "/realtime/v1/longpoll?apikey="+h.k.PublishableKey+"&token="+a); resp.StatusCode != 200 {
		t.Errorf("long poll after the keys are enabled again: %d", resp.StatusCode)
	}
}

// A WebSocket with the publishable key is the one thing the route accepts while the keys are off.
func TestRealtimeWebSocketAcceptedWhileLegacyKeysDisabled(t *testing.T) {
	h := disabledHarness(t)
	serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := h.dialRealtime(t, ctx, nil)
	defer c.CloseNow()
	echoOK(t, ctx, c, "hello")
}

// A failed key lookup during the recheck does not leave a tracked socket open for good.
func TestRealtimeRecheckRetriesAfterFailedLookup(t *testing.T) {
	h := newHarness(t)
	serveEchoRealtime(h)
	h.srv.recheckWait = 50 * time.Millisecond // retry budget (6 x 50 ms) well above the 60 ms outage below
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := h.dialRealtime(t, ctx, nil)
	defer c.CloseNow()
	echoOK(t, ctx, c, "one")
	k := *h.k
	k.LegacyDisabled = true
	h.keys.set(h.ref, &k)
	h.keys.setFail(errors.New("registry down"))
	h.srv.table.invalidateKeys(h.ref)
	time.Sleep(60 * time.Millisecond)
	if len(h.srv.sockets.refs(h.ref)) != 1 {
		t.Fatal("the socket was closed without knowing the keys")
	}
	h.keys.setFail(nil)
	eventually(t, "the retry closed the socket", func() bool { return len(h.srv.sockets.refs(h.ref)) == 0 })
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("socket still open")
	}
}
