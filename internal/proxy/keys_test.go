package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/supavise/supavise/internal/secrets"
)

const testRef = "abcdefghijklmnopqrst"

func testKeys(t *testing.T, ref string) *secrets.ProjectKeys {
	t.Helper()
	k, err := secrets.NewProjectKeys(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func signJWT(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestClassify(t *testing.T) {
	k := testKeys(t, testRef)
	other := testKeys(t, "zyxwvutsrqponmlkjihg")
	exp := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name   string
		apikey string
		role   string
		jwt    string
		ok     bool
	}{
		{"empty", "", "", "", false},
		{"publishable -> anon JWT", k.PublishableKey, secrets.RoleAnon, k.AnonKey, true},
		{"secret -> service_role JWT", k.SecretKey, secrets.RoleServiceRole, k.ServiceRoleKey, true},
		{"legacy anon", k.AnonKey, secrets.RoleAnon, k.AnonKey, true},
		{"legacy service_role", k.ServiceRoleKey, secrets.RoleServiceRole, k.ServiceRoleKey, true},
		{"other project's publishable", other.PublishableKey, "", "", false},
		{"other project's secret", other.SecretKey, "", "", false},
		{"other project's legacy anon", other.AnonKey, "", "", false},
		{"unknown sb_ key", "sb_publishable_nope", "", "", false},
		{"sb_ prefix only", "sb_", "", "", false},
		{"garbage", "not-a-key", "", "", false},
		{"publishable with suffix", k.PublishableKey + "x", "", "", false},
		{"truncated secret", k.SecretKey[:len(k.SecretKey)-1], "", "", false},
		{"re-signed anon, same secret, no ref", signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "anon", "exp": exp}), secrets.RoleAnon, "", true},
		{"re-signed service_role with matching ref", signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp}), secrets.RoleServiceRole, "", true},
		{"ref claim of another project", signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "anon", "ref": "zyxwvutsrqponmlkjihg", "exp": exp}), "", "", false},
		{"ref claim not a string", signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "anon", "ref": 7, "exp": exp}), "", "", false},
		{"signed with another secret", signJWT(t, other.JWTSecret, jwt.MapClaims{"role": "anon", "ref": testRef, "exp": exp}), "", "", false},
		{"authenticated role is not an API key", signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "authenticated", "ref": testRef, "exp": exp}), "", "", false},
		{"no role", signJWT(t, k.JWTSecret, jwt.MapClaims{"ref": testRef, "exp": exp}), "", "", false},
		{"expired", signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "anon", "ref": testRef, "exp": time.Now().Add(-time.Hour).Unix()}), "", "", false},
		{"alg none", func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"role": "service_role", "ref": testRef}).SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}(), "", "", false},
	}
	for _, c := range cases {
		role, j, ok := classify(k, testRef, c.apikey)
		if ok != c.ok || role != c.role {
			t.Errorf("%s: got (%q, ok=%v), want (%q, ok=%v)", c.name, role, ok, c.role, c.ok)
			continue
		}
		if c.ok && c.jwt != "" && j != c.jwt {
			t.Errorf("%s: forwarded JWT mismatch", c.name)
		}
		if c.ok && j == "" {
			t.Errorf("%s: empty forwarded JWT", c.name)
		}
	}
	if _, _, ok := classify(nil, testRef, k.AnonKey); ok {
		t.Error("nil keys accepted a key")
	}
	if _, _, ok := classify(&secrets.ProjectKeys{}, testRef, "sb_publishable_"); ok {
		t.Error("empty keys matched an empty configured key")
	}
}

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestAuthorizeTranslationTable(t *testing.T) {
	k := testKeys(t, testRef)
	byName := func(n string) *route {
		for i := range projectRoutes {
			if projectRoutes[i].name == n {
				return &projectRoutes[i]
			}
		}
		t.Fatalf("no route %s", n)
		return nil
	}
	rest := byName("rest-v1")
	realUserJWT := "Bearer eyJhbGciOi.user.jwt"

	cases := []struct {
		name      string
		rt        *route
		h         http.Header
		query     string
		status    int
		body      string
		wantSet   map[string]string // exact expectations for these headers
		wantQuery string
	}{
		{name: "rest: no key", rt: rest, h: hdr(), status: 401, body: "Unauthorized"},
		{name: "rest: invalid key", rt: rest, h: hdr("apikey", "sb_publishable_wrong"), status: 401, body: "Unauthorized"},
		{name: "rest: publishable", rt: rest, h: hdr("apikey", k.PublishableKey),
			wantSet: map[string]string{"Apikey": k.AnonKey, "Authorization": "Bearer " + k.AnonKey}},
		{name: "rest: secret", rt: rest, h: hdr("apikey", k.SecretKey),
			wantSet: map[string]string{"Apikey": k.ServiceRoleKey, "Authorization": "Bearer " + k.ServiceRoleKey}},
		{name: "rest: legacy anon", rt: rest, h: hdr("apikey", k.AnonKey),
			wantSet: map[string]string{"Apikey": k.AnonKey, "Authorization": "Bearer " + k.AnonKey}},
		{name: "rest: sb_ key also in Authorization", rt: rest, h: hdr("apikey", k.SecretKey, "Authorization", "Bearer "+k.SecretKey),
			wantSet: map[string]string{"Apikey": k.ServiceRoleKey, "Authorization": "Bearer " + k.ServiceRoleKey}},
		{name: "rest: sb_ key only in Authorization", rt: rest, h: hdr("Authorization", "Bearer "+k.PublishableKey),
			wantSet: map[string]string{"Apikey": k.AnonKey, "Authorization": "Bearer " + k.AnonKey}},
		{name: "rest: user JWT kept, apikey translated", rt: rest, h: hdr("apikey", k.PublishableKey, "Authorization", realUserJWT),
			wantSet: map[string]string{"Apikey": k.AnonKey}},
		{name: "rest: lowercase bearer scheme is a real JWT", rt: rest, h: hdr("apikey", k.AnonKey, "Authorization", "bearer user.jwt"),
			wantSet: map[string]string{"Apikey": k.AnonKey}},
		{name: "rest: non-bearer Authorization replaced", rt: rest, h: hdr("apikey", k.AnonKey, "Authorization", "Basic abc"),
			wantSet: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "rest: apikey in query, stripped", rt: rest, h: hdr(), query: "select=*&apikey=" + k.PublishableKey, wantQuery: "select=*",
			wantSet: map[string]string{"Apikey": k.AnonKey, "Authorization": "Bearer " + k.AnonKey}},
		{name: "rest: header wins over query, query still stripped", rt: rest, h: hdr("apikey", k.PublishableKey), query: "apikey=sb_publishable_other&a=1", wantQuery: "a=1",
			wantSet: map[string]string{"Apikey": k.AnonKey}},
		{name: "rest: invalid query key", rt: rest, h: hdr(), query: "apikey=nope", status: 401, body: "Unauthorized"},
		{name: "rest openapi root: anon forbidden", rt: byName("rest-v1-openapi"), h: hdr("apikey", k.PublishableKey), status: 403, body: "RBAC: access denied"},
		{name: "rest openapi root: legacy anon forbidden", rt: byName("rest-v1-openapi"), h: hdr("apikey", k.AnonKey), status: 403, body: "RBAC: access denied"},
		{name: "rest openapi root: no key", rt: byName("rest-v1-openapi"), h: hdr(), status: 401, body: "Unauthorized"},
		{name: "rest openapi root: secret", rt: byName("rest-v1-openapi"), h: hdr("apikey", k.SecretKey),
			wantSet: map[string]string{"Apikey": k.ServiceRoleKey}},
		{name: "auth protected: no key", rt: byName("auth-v1"), h: hdr(), status: 401, body: "Unauthorized"},
		{name: "auth protected: publishable", rt: byName("auth-v1"), h: hdr("apikey", k.PublishableKey),
			wantSet: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "auth open: no key passes", rt: byName("auth-v1-verify"), h: hdr()},
		{name: "auth open: invalid key passes untouched", rt: byName("auth-v1-callback"), h: hdr("apikey", "sb_publishable_bad")},
		{name: "auth open: valid key translated", rt: byName("auth-v1-authorize"), h: hdr("apikey", k.PublishableKey),
			wantSet: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "storage: no key passes (public objects)", rt: byName("storage-v1"), h: hdr()},
		{name: "storage: secret translated", rt: byName("storage-v1"), h: hdr("apikey", k.SecretKey),
			wantSet: map[string]string{"Authorization": "Bearer " + k.ServiceRoleKey}},
		{name: "storage: user JWT kept", rt: byName("storage-v1"), h: hdr("apikey", k.PublishableKey, "Authorization", realUserJWT)},
		{name: "storage s3: SigV4 untouched", rt: byName("storage-v1-s3"), h: hdr("Authorization", "AWS4-HMAC-SHA256 Credential=x"), query: "apikey=" + k.PublishableKey, wantQuery: "apikey=" + k.PublishableKey},
		{name: "graphql: publishable", rt: byName("graphql-v1"), h: hdr("apikey", k.PublishableKey),
			wantSet: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "realtime api: no key", rt: byName("realtime-v1-api"), h: hdr(), status: 401, body: "Unauthorized"},
		{name: "realtime api: publishable", rt: byName("realtime-v1-api"), h: hdr("apikey", k.PublishableKey),
			wantSet: map[string]string{"Authorization": "Bearer " + k.AnonKey}},
		{name: "realtime ws: query key becomes JWT in query and x-api-key", rt: byName("realtime-v1-ws"), h: hdr(), query: "vsn=1.0.0&apikey=" + k.PublishableKey,
			wantSet:   map[string]string{"Apikey": k.AnonKey, "X-Api-Key": k.AnonKey},
			wantQuery: "vsn=1.0.0&apikey=" + k.AnonKey},
		{name: "realtime ws: client x-api-key is dropped, Authorization untouched", rt: byName("realtime-v1-ws"), h: hdr("apikey", k.SecretKey, "X-Api-Key", "evil", "Authorization", realUserJWT),
			wantSet: map[string]string{"X-Api-Key": k.ServiceRoleKey}, wantQuery: "apikey=" + k.ServiceRoleKey},
		{name: "realtime ws: invalid", rt: byName("realtime-v1-ws"), h: hdr("X-Api-Key", k.AnonKey), query: "apikey=bad", status: 401, body: "Unauthorized"},
	}
	for _, c := range cases {
		res := authorize(c.rt, k, testRef, c.h, c.query)
		if res.status != c.status || (c.status != 0 && res.body != c.body) {
			t.Errorf("%s: status %d %q, want %d %q", c.name, res.status, res.body, c.status, c.body)
			continue
		}
		if c.status != 0 {
			continue
		}
		for name, want := range c.wantSet {
			if got := res.set[name]; got != want {
				t.Errorf("%s: set[%s] = %q, want %q", c.name, name, got, want)
			}
		}
		if res.rawQuery != c.wantQuery {
			t.Errorf("%s: rawQuery %q, want %q", c.name, res.rawQuery, c.wantQuery)
		}
	}
}

func TestAuthorizeNoAuthorizationFromInvalidKeyOnOpenRoute(t *testing.T) {
	k := testKeys(t, testRef)
	rt := matchRoute("/storage/v1/object/x")
	res := authorize(rt, k, testRef, hdr("apikey", "sb_secret_bad"), "")
	if res.status != 0 || len(res.set) != 0 {
		t.Fatalf("invalid key on open route must not be translated: %+v", res)
	}
}

func TestAuthorizeFunctions(t *testing.T) {
	k := testKeys(t, testRef)
	rt := matchRoute("/functions/v1/hello")
	userJWT := "Bearer user.jwt.token"
	cases := []struct {
		name    string
		h       http.Header
		status  int
		body    string
		sbAPI   string // expected sb-api-key, "" for none
		wantDel bool
	}{
		{name: "no credentials passes", h: hdr()},
		{name: "publishable in apikey", h: hdr("apikey", k.PublishableKey), sbAPI: k.AnonKey},
		{name: "secret in apikey", h: hdr("apikey", k.SecretKey), sbAPI: k.ServiceRoleKey},
		{name: "sb_ key in bearer fallback", h: hdr("Authorization", "Bearer "+k.PublishableKey), sbAPI: k.AnonKey},
		{name: "apikey and same bearer", h: hdr("apikey", k.SecretKey, "Authorization", "Bearer "+k.SecretKey), sbAPI: k.ServiceRoleKey},
		{name: "conflicting sb_ bearer and apikey", h: hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+k.SecretKey), status: 401, body: "Conflicting API keys"},
		{name: "unknown sb_ key", h: hdr("apikey", "sb_secret_unknown"), status: 401, body: "Invalid API key"},
		{name: "legacy jwt passes to the runtime", h: hdr("apikey", k.AnonKey, "Authorization", "Bearer "+k.AnonKey)},
		{name: "user jwt passes", h: hdr("apikey", k.PublishableKey, "Authorization", userJWT), sbAPI: k.AnonKey},
		{name: "client sb-api-key spoof dropped", h: hdr("Sb-Api-Key", "forged")},
	}
	for _, c := range cases {
		res := authorize(rt, k, testRef, c.h, "x=1&apikey=left-alone")
		if res.status != c.status || res.body != c.body {
			t.Errorf("%s: status %d %q, want %d %q", c.name, res.status, res.body, c.status, c.body)
			continue
		}
		if c.status != 0 {
			continue
		}
		if res.set["Sb-Api-Key"] != c.sbAPI {
			t.Errorf("%s: sb-api-key %q, want %q", c.name, res.set["Sb-Api-Key"], c.sbAPI)
		}
		dropped := false
		for _, d := range res.del {
			dropped = dropped || d == "Sb-Api-Key"
		}
		if !dropped {
			t.Errorf("%s: client sb-api-key not dropped", c.name)
		}
		if res.rawQuery != "x=1&apikey=left-alone" {
			t.Errorf("%s: functions query must be untouched, got %q", c.name, res.rawQuery)
		}
		if _, ok := res.set["Authorization"]; ok {
			t.Errorf("%s: functions must not touch Authorization", c.name)
		}
	}
	// A project without opaque keys passes everything through.
	legacy := &secrets.ProjectKeys{JWTSecret: k.JWTSecret, AnonKey: k.AnonKey, ServiceRoleKey: k.ServiceRoleKey}
	if res := authorize(rt, legacy, testRef, hdr("apikey", "sb_secret_x"), ""); res.status != 0 {
		t.Errorf("legacy-only project: %+v", res)
	}
}

func TestEqConst(t *testing.T) {
	if !eqConst("abc", "abc") || eqConst("abc", "abd") || eqConst("abc", "") || eqConst("", "") || eqConst("abc", "abcd") {
		t.Fatal("eqConst")
	}
}
