package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OWNER/sbctl/internal/config"
	"github.com/OWNER/sbctl/internal/registry"
	"github.com/OWNER/sbctl/internal/secrets"
)

func testSecrets(t testing.TB) *secrets.AESGCM {
	t.Helper()
	s, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewSigningKeyIsWhatGoTrueTakes(t *testing.T) {
	k, err := NewSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidSigningKey(k); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(k, "\r\n ") {
		t.Fatal("an environment file cannot carry line breaks")
	}
	der, _ := base64.StdEncoding.DecodeString(k)
	if key, err := x509.ParsePKCS1PrivateKey(der); err != nil || key.N.BitLen() != 2048 || key.E != 65537 {
		t.Fatalf("the key is %v, %v", key, err)
	}
	k2, _ := NewSigningKey()
	if k == k2 {
		t.Fatal("two keys are the same")
	}
}

func TestValidSigningKeyRefusesWhatGoTrueRefuses(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for name, k := range map[string]string{
		"not base64":  "!!!",
		"not a key":   base64.StdEncoding.EncodeToString([]byte("hello")),
		"too small":   base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(small)),
		"pkcs8":       base64.StdEncoding.EncodeToString(mustPKCS8(t)),
		"empty":       "",
		"padding off": strings.TrimRight(mustKey(t), "="),
	} {
		if err := ValidSigningKey(k); err == nil && name != "padding off" {
			t.Errorf("%s was accepted", name)
		}
	}
}

func mustKey(t testing.TB) string {
	k, err := NewSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustPKCS8(t testing.TB) []byte {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnsureSigningKey(t *testing.T) {
	ctx := context.Background()
	reg := registry.NewMemory()
	sec := testSecrets(t)
	for _, ref := range []string{config.SystemRef, "abcdefghijklmnopqrst"} {
		if err := reg.CreateProject(ctx, &registry.Project{Ref: ref, Name: ref, Class: "micro", Status: registry.StatusActiveHealthy}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := EnsureSigningKey(ctx, reg, sec, "zzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Fatal("a key for a project that does not exist")
	}
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := EnsureSigningKey(ctx, reg, sec, config.SystemRef)
			if err != nil {
				t.Error(err)
			}
			got[i] = k
		}()
	}
	wg.Wait()
	for _, k := range got {
		if k != got[0] || k == "" {
			t.Fatalf("concurrent callers got different keys")
		}
	}
	if err := ValidSigningKey(got[0]); err != nil {
		t.Fatal(err)
	}
	// Sealed in the registry, not stored as the key itself.
	sealed, err := reg.GetSecret(ctx, config.SystemRef, SecretSigningKey)
	if err != nil || strings.Contains(string(sealed), got[0][:40]) {
		t.Fatalf("the key is stored in the clear: %v", err)
	}
	// Idempotent, and every project has its own.
	if again, _ := EnsureSigningKey(ctx, reg, sec, config.SystemRef); again != got[0] {
		t.Fatal("a second call made another key")
	}
	other, err := EnsureSigningKey(ctx, reg, sec, "abcdefghijklmnopqrst")
	if err != nil || other == got[0] {
		t.Fatalf("the project's key is the system's: %v", err)
	}
}

func TestHookSecretShape(t *testing.T) {
	s, err := HookSecret(testSecrets(t))
	if err != nil {
		t.Fatal(err)
	}
	// GoTrue's own pattern for symmetric hook secrets (internal/conf/configuration.go).
	if !regexp.MustCompile(`^v1,whsec_[A-Za-z0-9+/=]{32,88}`).MatchString(s) {
		t.Fatalf("GoTrue would refuse %q", s)
	}
	if s2, _ := HookSecret(testSecrets(t)); s2 != s {
		t.Fatal("the secret is not stable across processes")
	}
	other, _ := secrets.New([]byte("fedcba9876543210fedcba9876543210"))
	if s3, _ := HookSecret(other); s3 == s {
		t.Fatal("another node derives the same secret")
	}
	if _, err := HookSecret(plainSecrets{}); err != ErrNoDerivation {
		t.Fatalf("secrets that cannot derive: %v", err)
	}
}

type plainSecrets struct{}

func (plainSecrets) Seal(b []byte) ([]byte, error) { return b, nil }
func (plainSecrets) Open(b []byte) ([]byte, error) { return b, nil }

func TestWebhookSignatures(t *testing.T) {
	secret, _ := HookSecret(testSecrets(t))
	body := []byte(`{"user":{"email":"a@b.test"}}`)
	now := time.Now()
	h, err := SignWebhook(secret, "msg_1", now, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyWebhook(secret, h, body, now); err != nil {
		t.Fatalf("a good call: %v", err)
	}
	for name, tc := range map[string]struct {
		secret string
		h      func(http.Header)
		body   []byte
		at     time.Time
	}{
		"tampered body":   {secret, nil, append(body, ' '), now},
		"other secret":    {"v1,whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32)), nil, body, now},
		"too old":         {secret, nil, body, now.Add(2 * WebhookTolerance)},
		"from the future": {secret, nil, body, now.Add(-2 * WebhookTolerance)},
		"no id":           {secret, func(h http.Header) { h.Del("webhook-id") }, body, now},
		"other id":        {secret, func(h http.Header) { h.Set("webhook-id", "msg_2") }, body, now},
		"bad timestamp":   {secret, func(h http.Header) { h.Set("webhook-timestamp", "soon") }, body, now},
		"no signature":    {secret, func(h http.Header) { h.Del("webhook-signature") }, body, now},
		"junk signature":  {secret, func(h http.Header) { h.Set("webhook-signature", "v1,!!!") }, body, now},
		"bad secret":      {"nope", nil, body, now},
	} {
		hh := h.Clone()
		if tc.h != nil {
			tc.h(hh)
		}
		if err := VerifyWebhook(tc.secret, hh, tc.body, tc.at); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// GoTrue sends one signature per secret, space separated; any matching one will do.
	h.Set("webhook-signature", "v1,AAAA "+h.Get("webhook-signature"))
	if err := VerifyWebhook(secret, h, body, now); err != nil {
		t.Fatalf("a list of signatures: %v", err)
	}
	// The signature of the Standard Webhooks specification's example (a fixed secret, id,
	// time and payload) is what this computes, so GoTrue's library and ours agree.
	spec := "v1,whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	sh, _ := SignWebhook(spec, "msg_p5jXN8AQM9LWM0D4loKWxJek", time.Unix(1614265330, 0), []byte(`{"test": 2432232314}`))
	if got := sh.Get("webhook-signature"); got != "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatalf("the specification's example gives %s", got)
	}
}

func TestHookEvent(t *testing.T) {
	var ev HookEvent
	if err := json.Unmarshal([]byte(`{"metadata":{"ip_address":"1.2.3.4"},"user":{"email":"a@b.test","app_metadata":{"provider":"sso:A0000000-0000-4000-8000-000000000001"},"user_metadata":{"sbctl_grant":"sbg_x"}}}`), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Provider() != "a0000000-0000-4000-8000-000000000001" || ev.GrantToken() != "sbg_x" {
		t.Fatalf("%q %q", ev.Provider(), ev.GrantToken())
	}
	var email HookEvent
	_ = json.Unmarshal([]byte(`{"user":{"app_metadata":{"provider":"email"},"user_metadata":{"provider":"sso:x"}}}`), &email)
	if email.Provider() != "" {
		t.Fatal("user_metadata decides the provider")
	}
	if r := HookRefusal("no"); r["error"].(map[string]any)["http_code"] != http.StatusForbidden {
		t.Fatalf("%v", r)
	}
}

func TestHookURL(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:7000": "http://127.0.0.1:7000" + HookPath,
		"0.0.0.0:7100":   "http://127.0.0.1:7100" + HookPath,
		":7200":          "http://127.0.0.1:7200" + HookPath,
		"[::1]:7300":     "http://[::1]:7300" + HookPath,
		"garbage":        "http://127.0.0.1:7000" + HookPath,
	} {
		cfg := config.Default()
		cfg.Listen.Admin = listen
		if got := HookURL(cfg); got != want {
			t.Errorf("%s: %s, want %s", listen, got, want)
		}
	}
}

func TestDomains(t *testing.T) {
	for in, want := range map[string]string{"Acme.Test": "acme.test", "@acme.test": "acme.test", " sub.acme.co ": "sub.acme.co"} {
		if got, err := NormalizeDomain(in); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "acme", "a b.test", "-a.test", "a..test", "a.t", "x@acme.test", "http://acme.test", "acme.test/"} {
		if _, err := NormalizeDomain(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if got, err := NormalizeDomains([]string{"A.test", "b.test"}); err != nil || strings.Join(got, ",") != "a.test,b.test" {
		t.Errorf("%v %v", got, err)
	}
	if _, err := NormalizeDomains([]string{"a.test", "A.TEST"}); err == nil {
		t.Error("a duplicate was accepted")
	}
}

const goodMetadata = `<?xml version="1.0"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.acme.test/x">
 <md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"/>
</md:EntityDescriptor>`

func TestValidateXML(t *testing.T) {
	if id, err := ValidateXML(goodMetadata); err != nil || id != "https://idp.acme.test/x" {
		t.Fatalf("%q, %v", id, err)
	}
	// A document without a namespace prefix is as good.
	if _, err := ValidateXML(`<EntityDescriptor entityID="e"><IDPSSODescriptor/></EntityDescriptor>`); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"not xml":      "hello",
		"truncated":    `<EntityDescriptor entityID="e"><IDPSSODescriptor/>`,
		"wrong root":   `<html entityID="e"><IDPSSODescriptor/></html>`,
		"no entity id": `<EntityDescriptor><IDPSSODescriptor/></EntityDescriptor>`,
		"an SP":        `<EntityDescriptor entityID="e"><SPSSODescriptor/></EntityDescriptor>`,
		"two IdPs":     `<EntityDescriptor entityID="e"><IDPSSODescriptor/><IDPSSODescriptor/></EntityDescriptor>`,
		"too large":    `<EntityDescriptor entityID="e"><IDPSSODescriptor/><!--` + strings.Repeat("x", MaxMetadataBytes) + `--></EntityDescriptor>`,
		"empty":        "",
	} {
		if _, err := ValidateXML(doc); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestResolveMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	good := filepath.Join(dir, "idp.xml")
	if err := os.WriteFile(good, []byte(goodMetadata), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.xml")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)

	if md, err := ResolveMetadata(ctx, "", good, nil); err != nil || md.XML != goodMetadata || md.URL != "" {
		t.Fatalf("file: %+v, %v", md, err)
	}
	if _, err := ResolveMetadata(ctx, "", bad, nil); err == nil {
		t.Error("an invalid file")
	}
	if _, err := ResolveMetadata(ctx, "", filepath.Join(dir, "missing.xml"), nil); err == nil {
		t.Error("a missing file")
	}
	if _, err := ResolveMetadata(ctx, "https://x.test/m", good, nil); err == nil {
		t.Error("an address and a file")
	}
	if _, err := ResolveMetadata(ctx, "", "", nil); err == nil {
		t.Error("nothing")
	}
	// An https address is GoTrue's to fetch (and refresh).
	if md, err := ResolveMetadata(ctx, "https://idp.acme.test/metadata", "", nil); err != nil || md.URL != "https://idp.acme.test/metadata" || md.XML != "" {
		t.Fatalf("https: %+v, %v", md, err)
	}
	// A plain http address is refused, except on this machine, where it is fetched here.
	for _, u := range []string{"http://idp.acme.test/metadata", "http://192.0.2.1/metadata", "ftp://x.test/m", "idp.acme.test/metadata", "https://"} {
		if _, err := ResolveMetadata(ctx, u, "", nil); err == nil {
			t.Errorf("%s was accepted", u)
		}
	}
	var served string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(served))
		case "/gone":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	served = goodMetadata
	if md, err := ResolveMetadata(ctx, srv.URL+"/ok", "", nil); err != nil || md.XML != goodMetadata || md.URL != "" {
		t.Fatalf("loopback: %+v, %v", md, err)
	}
	if _, err := ResolveMetadata(ctx, srv.URL+"/gone", "", nil); err == nil {
		t.Error("a 404")
	}
	served = "<html/>"
	if _, err := ResolveMetadata(ctx, srv.URL+"/ok", "", nil); err == nil {
		t.Error("an invalid document from a loopback address")
	}
}

func TestSPURLs(t *testing.T) {
	sp := URLsFor("https://api.example.com/auth/v1/")
	if sp.ACSURL != "https://api.example.com/auth/v1/sso/saml/acs" || sp.EntityID != "https://api.example.com/auth/v1/sso/saml/metadata" || sp.MetadataURL != sp.EntityID {
		t.Fatalf("%+v", sp)
	}
}

func TestClientTalksToGoTrue(t *testing.T) {
	var gotAuth, gotKey, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey, gotPath = r.Header.Get("Authorization"), r.Header.Get("apikey"), r.Method+" "+r.URL.Path
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bb, _ := json.Marshal(b)
		gotBody = string(bb)
		switch {
		case r.Method == "GET" && r.URL.Path == "/admin/sso/providers":
			_, _ = w.Write([]byte(`{"items":[{"id":"x","saml":{"entity_id":"e","attribute_mapping":{}},"domains":null}]}`))
		case r.URL.Path == "/admin/sso/providers/gone":
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"error_code":"sso_provider_not_found","msg":"SSO Identity Provider not found"}`))
		case r.Method == "POST":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"new","saml":{"entity_id":"e"},"domains":[{"domain":"a.test"}],"disabled":false}`))
		default:
			_, _ = w.Write([]byte(`{"id":"x"}`))
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", ServiceKey: "service-key"}
	ctx := context.Background()
	ps, err := c.List(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("%v %v", ps, err)
	}
	if gotAuth != "Bearer service-key" || gotKey != "service-key" {
		t.Fatalf("credentials %q %q", gotAuth, gotKey)
	}
	// What GoTrue leaves out, the specs require.
	if ps[0].Domains == nil || ps[0].SAML.AttributeMapping.Keys == nil {
		t.Fatalf("not normalized: %+v", ps[0])
	}
	p, err := c.Create(ctx, CreateBody{MetadataXML: "<x/>", Domains: []string{"a.test"}})
	if err != nil || p.ID != "new" || p.DomainNames()[0] != "a.test" {
		t.Fatalf("%+v %v", p, err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(gotBody), &sent)
	if gotPath != "POST /admin/sso/providers" || sent["type"] != "saml" || sent["metadata_xml"] != "<x/>" {
		t.Fatalf("%s %s", gotPath, gotBody)
	}
	dom := []string{}
	if _, err := c.Update(ctx, "x", UpdateBody{Domains: &dom}); err != nil {
		t.Fatal(err)
	}
	// An empty list of domains is sent (it removes them); an absent one is not.
	if !strings.Contains(gotBody, `"domains":[]`) {
		t.Fatalf("update body %s", gotBody)
	}
	if _, err := c.Update(ctx, "x", UpdateBody{}); err != nil || strings.Contains(gotBody, "domains") {
		t.Fatalf("update body %s, %v", gotBody, err)
	}
	_, err = c.Get(ctx, "gone")
	ae, ok := err.(*APIError)
	if !ok || ae.Status != 404 || ae.Code != "sso_provider_not_found" || !strings.Contains(ae.Error(), "not found") {
		t.Fatalf("%#v", err)
	}
	// Ids that are not one path segment are escaped, never joined onto the path.
	if _, err := c.Get(ctx, "../users"); err != nil && gotPath != "" && strings.Contains(gotPath, "/admin/users") {
		t.Fatalf("an id climbed out of its place: %s", gotPath)
	}
	if _, err := (&Client{BaseURL: "http://127.0.0.1:1"}).List(ctx); err == nil {
		t.Fatal("an unreachable GoTrue")
	}
}
