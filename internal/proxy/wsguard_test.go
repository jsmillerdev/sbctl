package proxy

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"github.com/jsmillerdev/supavise/internal/secrets"
)

// ---- S3 ----

func TestLegacyDisabledOnS3(t *testing.T) {
	k := testKeys(t, testRef)
	k.LegacyDisabled = true
	exp := time.Now().Add(time.Hour).Unix()
	resigned := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp})
	user := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u1", "ref": testRef, "exp": exp})
	sigv4 := "AWS4-HMAC-SHA256 Credential=" + testRef + "/20260101/local/s3/aws4_request, SignedHeaders=host, Signature=abc"

	rt := matchRoute("/storage/v1/s3/bucket/obj")
	if rt == nil || rt.keys != keyS3 {
		t.Fatalf("S3 route: %+v", rt)
	}
	type c struct {
		h     http.Header
		query string
	}
	for name, c := range map[string]c{
		"service_role in the header":          {h: hdr("Authorization", sigv4, "X-Amz-Security-Token", k.ServiceRoleKey)},
		"anon in the header":                  {h: hdr("Authorization", sigv4, "x-amz-security-token", k.AnonKey)},
		"re-signed service_role in header":    {h: hdr("Authorization", sigv4, "X-Amz-Security-Token", resigned)},
		"service_role in the presigned query": {h: hdr(), query: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Security-Token=" + k.ServiceRoleKey + "&X-Amz-Signature=abc"},
		"lower-case query name":               {h: hdr(), query: "x-amz-security-token=" + k.ServiceRoleKey},
		"escaped query name and value":        {h: hdr(), query: "X-Amz-Security%2DToken=" + strings.ReplaceAll(k.AnonKey, ".", "%2E")},
		"anon after a user session in query":  {h: hdr(), query: "X-Amz-Security-Token=" + user + "&x-amz-security-token=" + k.AnonKey},
		"legacy key as bearer":                {h: hdr("Authorization", "Bearer "+k.ServiceRoleKey)},
		"multipart POST upload":               {h: hdr("Content-Type", "multipart/form-data; boundary=xyz")},
	} {
		res := authorize(rt, k, testRef, c.h, c.query)
		if res.status != http.StatusForbidden || res.ctype != "application/xml" ||
			!strings.Contains(res.body, "<Code>AccessDenied</Code>") || !strings.HasPrefix(res.body, "<?xml") {
			t.Errorf("S3, %s: %d %q %q", name, res.status, res.ctype, res.body)
		}
	}
	for name, c := range map[string]c{
		"S3 access key, no session token": {h: hdr("Authorization", sigv4)},
		"presigned, no session token":     {h: hdr(), query: "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=abc"},
		"user session as token (header)":  {h: hdr("Authorization", sigv4, "X-Amz-Security-Token", user)},
		"user session as token (query)":   {h: hdr(), query: "X-Amz-Security-Token=" + user},
		"opaque key as token":             {h: hdr("Authorization", sigv4, "X-Amz-Security-Token", k.SecretKey)},
		"octet-stream upload":             {h: hdr("Authorization", sigv4, "Content-Type", "application/octet-stream")},
	} {
		res := authorize(rt, k, testRef, c.h, c.query)
		if res.status != 0 || res.rawQuery != c.query || len(res.set) != 0 || len(res.del) != 0 {
			t.Errorf("S3, %s: refused or rewritten: %+v", name, res)
		}
	}
	// Enabled again, the legacy keys work as session tokens as before.
	k.LegacyDisabled = false
	if res := authorize(rt, k, testRef, hdr("Authorization", sigv4, "X-Amz-Security-Token", k.ServiceRoleKey, "Content-Type", "multipart/form-data; boundary=x"), "x-amz-security-token="+k.AnonKey); res.status != 0 {
		t.Errorf("S3 with legacy keys enabled: %d", res.status)
	}
}

// Through the proxy: the refusal is an S3-style XML document, nothing reaches Storage, and a
// request without a legacy session token is forwarded with its signed parts untouched.
func TestLegacyDisabledOnS3ThroughProxy(t *testing.T) {
	h := newHarness(t)
	k := testKeys(t, h.ref)
	k.LegacyDisabled = true
	h.keys.set(h.ref, k)
	h.k = k
	sigv4 := "AWS4-HMAC-SHA256 Credential=" + h.ref + "/20260101/local/s3/aws4_request, SignedHeaders=host;x-amz-security-token, Signature=abc"

	before := h.ups[svcStorage].count()
	resp, body := h.project("GET", "/storage/v1/s3/bucket/a%2Bb", "Authorization", sigv4, "X-Amz-Security-Token", k.ServiceRoleKey)
	if resp.StatusCode != 403 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/xml") || !strings.Contains(body, "<Code>AccessDenied</Code>") {
		t.Fatalf("header token: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	resp, _ = h.project("GET", "/storage/v1/s3/bucket/obj?X-Amz-Security-Token="+k.ServiceRoleKey+"&X-Amz-Signature=abc")
	if resp.StatusCode != 403 {
		t.Fatalf("query token: %d", resp.StatusCode)
	}
	if got := h.ups[svcStorage].count(); got != before {
		t.Fatalf("Storage saw %d refused request(s)", got-before)
	}

	user := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u1", "ref": h.ref, "exp": time.Now().Add(time.Hour).Unix()})
	for _, hd := range [][]string{{"Authorization", sigv4}, {"Authorization", sigv4, "X-Amz-Security-Token", user}} {
		if resp, _ := h.project("GET", "/storage/v1/s3/bucket/a%2Bb", hd...); resp.StatusCode != 200 {
			t.Fatalf("allowed S3 request %v: %d", hd, resp.StatusCode)
		}
		up := h.ups[svcStorage].last(t)
		if up.Header.Get("Authorization") != sigv4 || up.RequestURI != "/s3/bucket/a%2Bb" {
			t.Errorf("S3 request changed on the way: %q %q", up.Header.Get("Authorization"), up.RequestURI)
		}
	}
}

// ---- WebSocket frames ----

// wsFrame builds a client-to-server frame.
func wsFrame(fin bool, op byte, payload []byte, masked bool, rsv byte) []byte {
	b0 := op | rsv<<4
	if fin {
		b0 |= 0x80
	}
	f := []byte{b0}
	var m byte
	if masked {
		m = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		f = append(f, m|byte(n))
	case n < 1<<16:
		f = append(f, m|126, byte(n>>8), byte(n))
	default:
		f = append(f, m|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if !masked {
		return append(f, payload...)
	}
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	f = append(f, key[:]...)
	for i, c := range payload {
		f = append(f, c^key[i&3])
	}
	return f
}

// feedChunks feeds the stream in randomly sized pieces, as reads would deliver it.
func feedChunks(w *wsInspector, stream []byte, rnd *rand.Rand) (found bool, reason string) {
	for len(stream) > 0 {
		n := 1 + rnd.Intn(40)
		if n > len(stream) {
			n = len(stream)
		}
		if f, r := w.feed(stream[:n]); f || r != "" {
			return f, r
		}
		stream = stream[n:]
	}
	return false, ""
}

func TestWSInspector(t *testing.T) {
	k := testKeys(t, testRef)
	needles := legacyNeedles(&secrets.ProjectKeys{AnonKey: k.AnonKey, ServiceRoleKey: k.ServiceRoleKey, LegacyDisabled: true})
	if len(needles) != 2 || legacyNeedles(k) != nil {
		t.Fatalf("needles: %d, enabled: %v", len(needles), legacyNeedles(k))
	}
	join := func(tok string) []byte {
		return []byte(`["1","1","realtime:x","phx_join",{"config":{},"access_token":"` + tok + `"}]`)
	}
	escaped := strings.Replace(k.ServiceRoleKey, "e", `e`, 1)
	escaped = strings.Replace(escaped, "J", `J`, 1)
	escaped = strings.Replace(escaped, ".", `.`, 2)
	padding := strings.Repeat("x", 70000)
	cat := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	half := len(k.ServiceRoleKey) / 2
	frag1 := []byte(`["1","1","t","access_token",{"access_token":"` + k.ServiceRoleKey[:half])
	frag2 := []byte(k.ServiceRoleKey[half:] + `"}]`)

	for name, c := range map[string]struct {
		stream []byte
		found  bool
		reason string
	}{
		"masked join with the anon key":      {stream: wsFrame(true, 1, join(k.AnonKey), true, 0), found: true},
		"masked join with service_role":      {stream: wsFrame(true, 1, join(k.ServiceRoleKey), true, 0), found: true},
		"unmasked frame":                     {stream: wsFrame(true, 1, join(k.ServiceRoleKey), false, 0), found: true},
		"JSON-escaped key":                   {stream: wsFrame(true, 1, join(escaped), true, 0), found: true},
		"key after a long benign frame":      {stream: cat(wsFrame(true, 1, []byte(padding), true, 0), wsFrame(true, 1, join(k.AnonKey), true, 0)), found: true},
		"key at the end of a 70 kB frame":    {stream: wsFrame(true, 1, append([]byte(padding), join(k.AnonKey)...), true, 0), found: true},
		"key split over two fragments":       {stream: cat(wsFrame(false, 1, frag1, true, 0), wsFrame(true, 0, frag2, true, 0)), found: true},
		"fragments with a ping between them": {stream: cat(wsFrame(false, 1, frag1, true, 0), wsFrame(true, 9, []byte("hi"), true, 0), wsFrame(true, 0, frag2, true, 0)), found: true},
		"compressed frame (RSV1)":            {stream: wsFrame(true, 1, join("x"), true, 4), reason: "reserved"},

		"publishable key in a join":          {stream: wsFrame(true, 1, join(k.PublishableKey), true, 0)},
		"user session in a join":             {stream: wsFrame(true, 1, join(signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u"})), true, 0)},
		"heartbeat":                          {stream: wsFrame(true, 1, []byte(`[null,"7","phoenix","heartbeat",{}]`), true, 0)},
		"empty frames and a close":           {stream: cat(wsFrame(true, 1, nil, true, 0), wsFrame(true, 8, []byte{0x03, 0xe8}, true, 0))},
		"binary frame is not inspected":      {stream: wsFrame(true, 2, join(k.AnonKey), true, 0)},
		"key in a binary fragment sequence":  {stream: cat(wsFrame(false, 2, frag1, true, 0), wsFrame(true, 0, frag2, true, 0))},
		"key in a ping payload":              {stream: wsFrame(true, 9, []byte(k.AnonKey[:100]), true, 0)},
		"cut signature is still the key":     {stream: wsFrame(true, 1, join(k.AnonKey[:len(k.AnonKey)-1]), true, 0), found: true},
		"signing input without its dot":      {stream: wsFrame(true, 1, join(k.AnonKey[:strings.LastIndexByte(k.AnonKey, '.')]), true, 0)},
		"keys in two separate messages":      {stream: cat(wsFrame(true, 1, []byte(k.AnonKey[:half]), true, 0), wsFrame(true, 1, []byte(k.AnonKey[half:]), true, 0))},
		"text after binary after key prefix": {stream: cat(wsFrame(true, 1, join("a"), true, 0), wsFrame(true, 2, []byte("zz"), true, 0), wsFrame(true, 1, join("b"), true, 0))},
	} {
		for seed := int64(1); seed <= 8; seed++ {
			rnd := rand.New(rand.NewSource(seed))
			found, reason := feedChunks(newWSInspector(needles), c.stream, rnd)
			if found != c.found || (c.reason == "") != (reason == "") || !strings.Contains(reason, c.reason) {
				t.Errorf("%s (chunks seed %d): found=%v reason=%q, want found=%v reason~%q", name, seed, found, reason, c.found, c.reason)
				break
			}
		}
		if found, _ := newWSInspector(needles).feed(c.stream); found != c.found {
			t.Errorf("%s (one read): found=%v, want %v", name, found, c.found)
		}
	}
}

// ---- WebSocket through the proxy ----

// realtimeUpstream makes the harness's Realtime echo every message and record what it received.
type realtimeUpstream struct {
	mu   sync.Mutex
	msgs []string
	hdr  http.Header
	done chan struct{}
}

func (u *realtimeUpstream) received() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.msgs...)
}

func serveEchoRealtime(h *harness) *realtimeUpstream {
	u := &realtimeUpstream{done: make(chan struct{}, 16)}
	rt := h.ups[svcRealtime]
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			body := ""
			rt.mu.Lock()
			body = rt.reqs[len(rt.reqs)-1].Body
			rt.mu.Unlock()
			u.mu.Lock()
			u.msgs = append(u.msgs, body)
			u.mu.Unlock()
			w.WriteHeader(200)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		defer func() { u.done <- struct{}{} }()
		c.SetReadLimit(1 << 20)
		u.mu.Lock()
		u.hdr = r.Header.Clone()
		u.mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		for {
			typ, msg, err := c.Read(ctx)
			if err != nil {
				return
			}
			u.mu.Lock()
			u.msgs = append(u.msgs, string(msg))
			u.mu.Unlock()
			if err := c.Write(ctx, typ, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}
	return u
}

func (h *harness) dialRealtime(t *testing.T, ctx context.Context, opts *websocket.DialOptions) *websocket.Conn {
	t.Helper()
	if opts == nil {
		opts = &websocket.DialOptions{}
	}
	opts.Host = h.host(h.ref)
	u := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/realtime/v1/websocket?apikey=" + h.k.PublishableKey + "&vsn=1.0.0"
	c, resp, err := websocket.Dial(ctx, u, opts)
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	c.SetReadLimit(1 << 20)
	return c
}

func echoOK(t *testing.T, ctx context.Context, c *websocket.Conn, msg string) {
	t.Helper()
	if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := c.Read(ctx)
	if err != nil || string(got) != "echo:"+msg {
		t.Fatalf("echo of %.40q: err=%v got=%.40q", msg, err, got)
	}
}

func disabledHarness(t *testing.T) *harness {
	h := newHarness(t)
	k := testKeys(t, h.ref)
	k.LegacyDisabled = true
	h.keys.set(h.ref, k)
	h.k = k
	return h
}

func TestRealtimeSocketRefusesLegacyKeysWhenDisabled(t *testing.T) {
	h := disabledHarness(t)
	up := serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	user := signJWT(t, h.k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u1", "ref": h.ref, "exp": time.Now().Add(time.Hour).Unix()})
	join := func(tok string) string {
		b, _ := json.Marshal([]any{"1", "1", "realtime:room", "phx_join", map[string]any{"config": map[string]any{}, "access_token": tok}})
		return string(b)
	}
	accessToken := func(tok string) string {
		b, _ := json.Marshal([]any{"1", "2", "realtime:room", "access_token", map[string]any{"access_token": tok}})
		return string(b)
	}

	// Frames that carry no legacy key stream as before, whatever their size, with the
	// publishable key, a user session, large broadcasts and a masked client frame in between.
	c := h.dialRealtime(t, ctx, &websocket.DialOptions{CompressionMode: websocket.CompressionContextTakeover})
	for _, m := range []string{"hello", join(h.k.PublishableKey), join(user), accessToken(user), strings.Repeat("big", 100000)} {
		echoOK(t, ctx, c, m)
	}
	c.CloseNow()
	<-up.done
	up.mu.Lock()
	ext := up.hdr.Get("Sec-Websocket-Extensions")
	up.mu.Unlock()
	if ext != "" {
		t.Errorf("a compression extension reached Realtime: %q", ext)
	}

	resigned := signJWT(t, h.k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": h.ref, "exp": time.Now().Add(time.Hour).Unix()})
	_ = resigned // signed with the project secret, which only someone who can already mint any key has
	escaped := strings.Replace(h.k.ServiceRoleKey, "eyJ", `eyJ`, 1)
	for name, msg := range map[string]string{
		"anon in phx_join":                      join(h.k.AnonKey),
		"service_role in phx_join":              join(h.k.ServiceRoleKey),
		"service_role in an access_token event": accessToken(h.k.ServiceRoleKey),
		"escaped service_role":                  `["1","3","realtime:room","access_token",{"access_token":"` + escaped + `"}]`,
		"key after 200 kB of padding":           `["1","4","realtime:room","broadcast",{"pad":"` + strings.Repeat("p", 200000) + `","access_token":"` + h.k.AnonKey + `"}]`,
	} {
		c := h.dialRealtime(t, ctx, nil)
		echoOK(t, ctx, c, `["1","0","phoenix","heartbeat",{}]`) // the socket works until the key is sent
		before := len(up.received())
		if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		_, _, err := c.Read(ctx)
		if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || !strings.Contains(err.Error(), "legacy API keys are disabled") {
			t.Errorf("%s: read error %v, want a 1008 close with the reason", name, err)
		}
		c.CloseNow()
		<-up.done
		if got := up.received(); len(got) != before {
			t.Errorf("%s: Realtime received the frame (%d new messages)", name, len(got)-before)
		}
	}
}

func TestRealtimeSocketLegacyKeysPassWhenEnabled(t *testing.T) {
	h := newHarness(t)
	up := serveEchoRealtime(h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := h.dialRealtime(t, ctx, &websocket.DialOptions{CompressionMode: websocket.CompressionContextTakeover})
	defer c.CloseNow()
	echoOK(t, ctx, c, `["1","1","realtime:x","phx_join",{"access_token":"`+h.k.ServiceRoleKey+`"}]`)
	_ = up
}
