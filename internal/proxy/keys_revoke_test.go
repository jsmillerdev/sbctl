package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jsmillerdev/supavise/internal/secrets"
)

func TestClassifyMultipleAndRevokedKeys(t *testing.T) {
	k := testKeys(t, testRef)
	extra := secrets.NewSecretKey()
	pubExtra := secrets.NewPublishableKey()
	now := time.Now()
	k.SetRecord(secrets.APIKeyRecord{ID: "extra-secret", Name: "ci", Type: secrets.KeyTypeSecret, Key: extra, CreatedAt: now})
	k.SetRecord(secrets.APIKeyRecord{ID: "extra-pub", Name: "web", Type: secrets.KeyTypePublishable, Key: pubExtra, CreatedAt: now})
	k.SetRecord(secrets.APIKeyRecord{ID: "gone", Name: "old", Type: secrets.KeyTypeSecret, Revoked: true, CreatedAt: now})
	for name, key := range map[string]string{"default secret": k.SecretKey, "extra secret": extra, "extra publishable": pubExtra} {
		if _, _, ok := classify(k, testRef, key); !ok {
			t.Errorf("%s was refused", name)
		}
	}
	if role, _, _ := classify(k, testRef, extra); role != secrets.RoleServiceRole {
		t.Errorf("extra secret key has role %q", role)
	}
	if role, _, _ := classify(k, testRef, pubExtra); role != secrets.RoleAnon {
		t.Errorf("extra publishable key has role %q", role)
	}

	// Revoking the default secret key: only that key stops working.
	id := secrets.KeyID(testRef, secrets.DefaultKeyName, secrets.KeyTypeSecret)
	k.SetRecord(secrets.APIKeyRecord{ID: id, Name: secrets.DefaultKeyName, Type: secrets.KeyTypeSecret, Default: true, Revoked: true})
	if _, _, ok := classify(k, testRef, k.SecretKey); ok {
		t.Error("revoked default secret key still accepted")
	}
	if _, _, ok := classify(k, testRef, extra); !ok {
		t.Error("revoking the default key took the other secret key down")
	}
	if _, _, ok := classify(k, testRef, k.PublishableKey); !ok {
		t.Error("revoking the default secret key took the publishable key down")
	}
}

func TestClassifyLegacyDisabled(t *testing.T) {
	k := testKeys(t, testRef)
	k.LegacyDisabled = true
	exp := time.Now().Add(time.Hour).Unix()
	for name, key := range map[string]string{
		"anon JWT":         k.AnonKey,
		"service_role JWT": k.ServiceRoleKey,
		"re-signed anon":   signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "anon", "ref": testRef, "exp": exp}),
		"re-signed service_role without the temporary claim": signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp}),
	} {
		if _, _, ok := classify(k, testRef, key); ok {
			t.Errorf("%s accepted with the legacy keys disabled", name)
		}
	}
	if role, _, ok := classify(k, testRef, k.PublishableKey); !ok || role != secrets.RoleAnon {
		t.Error("the publishable key must keep working with the legacy keys disabled")
	}
	if role, _, ok := classify(k, testRef, k.SecretKey); !ok || role != secrets.RoleServiceRole {
		t.Error("the secret key must keep working with the legacy keys disabled")
	}
	tmp := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp, secrets.TemporaryClaim: true})
	if _, _, ok := classify(k, testRef, tmp); !ok {
		t.Error("the dashboard's temporary key must keep working")
	}
	other := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": "zyxwvutsrqponmlkjihg", "exp": exp, secrets.TemporaryClaim: true})
	if _, _, ok := classify(k, testRef, other); ok {
		t.Error("a temporary key of another project was accepted")
	}
	k.LegacyDisabled = false
	if _, _, ok := classify(k, testRef, k.AnonKey); !ok {
		t.Error("legacy keys must work again once enabled")
	}
}

// A revoked or disabled key must stop working at once: the registry change reaches the
// proxy's key cache, which would otherwise keep serving the old set for keysTTL.
func TestRevocationTakesEffectThroughTheRegistry(t *testing.T) {
	sec, err := secrets.New(secrets.RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	var h *harness
	h = newHarness(t, func(o *Options) {
		o.Keys = RegistryKeys{Registry: o.Registry, Secrets: sec}
	})
	ctx := context.Background()
	put := func(name string, plain []byte) {
		t.Helper()
		blob, err := sec.Seal(plain)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.reg.PutSecret(ctx, h.ref, name, blob); err != nil {
			t.Fatal(err)
		}
	}
	for name, val := range h.k.Map() {
		put(name, []byte(val))
	}
	extra := secrets.NewSecretKey()
	rec := secrets.APIKeyRecord{ID: "11111111-2222-3333-4444-555555555555", Name: "ci", Type: secrets.KeyTypeSecret, Key: extra, CreatedAt: time.Now()}
	b, _ := secrets.MarshalRecord(rec)
	put(secrets.RecordSecretName(rec.ID), b)

	status := func(apikey string) int {
		resp, _ := h.project("GET", "/rest/v1/x", "apikey", apikey)
		return resp.StatusCode
	}
	waitStatus := func(apikey string, want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			got := status(apikey)
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("status %d, want %d", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitStatus(extra, 200)
	waitStatus(h.k.AnonKey, 200)

	rec.Revoked, rec.Key = true, ""
	b, _ = secrets.MarshalRecord(rec)
	put(secrets.RecordSecretName(rec.ID), b)
	waitStatus(extra, 401)
	waitStatus(h.k.SecretKey, 200) // the default secret key is another key

	put(secrets.NameLegacyKeys, secrets.MarshalLegacyState(false))
	waitStatus(h.k.AnonKey, 401)
	waitStatus(h.k.PublishableKey, 200)
	put(secrets.NameLegacyKeys, secrets.MarshalLegacyState(true))
	waitStatus(h.k.AnonKey, 200)
}

// With the legacy keys disabled, a legacy key must not get through on the functions route
// (the runtime would accept it) nor as the exact bearer of a protected route beside a valid
// opaque key. A user's session JWT, the temporary dashboard key and the opaque keys still do.
func TestLegacyDisabledOnFunctionsAndBearer(t *testing.T) {
	k := testKeys(t, testRef)
	k.LegacyDisabled = true
	exp := time.Now().Add(time.Hour).Unix()
	resigned := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "anon", "ref": testRef, "exp": exp})
	tmp := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp, secrets.TemporaryClaim: true})
	user := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u1", "ref": testRef, "exp": exp})

	fn := matchRoute("/functions/v1/hello")
	for name, h := range map[string]http.Header{
		"anon in apikey":            hdr("apikey", k.AnonKey),
		"service_role in apikey":    hdr("apikey", k.ServiceRoleKey),
		"re-signed anon in apikey":  hdr("apikey", resigned),
		"anon as bearer":            hdr("Authorization", "Bearer "+k.AnonKey),
		"service_role bearer":       hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+k.ServiceRoleKey),
		"anon apikey, user bearer":  hdr("apikey", k.AnonKey, "Authorization", "Bearer "+user),
		"legacy beside secret key":  hdr("apikey", k.SecretKey, "Authorization", "Bearer "+k.AnonKey),
		"resigned anon, no bearer":  hdr("apikey", resigned),
		"service_role w/o sb_ only": hdr("apikey", k.ServiceRoleKey, "Authorization", "Bearer "+k.ServiceRoleKey),
	} {
		if res := authorize(fn, k, testRef, h, ""); res.status != http.StatusUnauthorized {
			t.Errorf("functions, %s: status %d, want 401", name, res.status)
		}
	}
	for name, h := range map[string]http.Header{
		"publishable":                hdr("apikey", k.PublishableKey),
		"publishable, user session":  hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+user),
		"secret":                     hdr("apikey", k.SecretKey),
		"temporary dashboard key":    hdr("apikey", tmp),
		"no credentials":             hdr(),
		"user session only":          hdr("Authorization", "Bearer "+user),
		"publishable, temporary key": hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+tmp),
	} {
		if res := authorize(fn, k, testRef, h, ""); res.status != 0 {
			t.Errorf("functions, %s: refused (%d %s)", name, res.status, res.body)
		}
	}

	rest := matchRoute("/rest/v1/items")
	if res := authorize(rest, k, testRef, hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+k.ServiceRoleKey), ""); res.status != http.StatusUnauthorized {
		t.Errorf("rest: the exact legacy service_role key as bearer: status %d, want 401", res.status)
	}
	if res := authorize(rest, k, testRef, hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+user), ""); res.status != 0 {
		t.Errorf("rest: a user session must keep working: %d", res.status)
	}
	// Enabled again, nothing changes for the legacy keys.
	k.LegacyDisabled = false
	if res := authorize(fn, k, testRef, hdr("apikey", k.AnonKey), ""); res.status != 0 {
		t.Errorf("functions with legacy keys enabled: %d", res.status)
	}
	if res := authorize(rest, k, testRef, hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+k.ServiceRoleKey), ""); res.status != 0 {
		t.Errorf("rest with legacy keys enabled: %d", res.status)
	}
}

// With the legacy keys disabled, Storage (a route that needs no key at the gateway and
// verifies the JWT itself) must turn away the legacy keys in every position: as apikey, in
// the query, as the exact bearer alone, and as a bearer beside a valid opaque key. Public
// objects without credentials, users' sessions and the opaque keys keep working.
func TestLegacyDisabledOnStorage(t *testing.T) {
	k := testKeys(t, testRef)
	k.LegacyDisabled = true
	exp := time.Now().Add(time.Hour).Unix()
	resigned := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp})
	tmp := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "service_role", "ref": testRef, "exp": exp, secrets.TemporaryClaim: true})
	user := signJWT(t, k.JWTSecret, jwt.MapClaims{"role": "authenticated", "sub": "u1", "ref": testRef, "exp": exp})

	st := matchRoute("/storage/v1/bucket")
	if st == nil || st.keys == keyNone {
		t.Fatalf("storage route: %+v", st)
	}
	for name, c := range map[string]struct {
		h     http.Header
		query string
	}{
		"service_role apikey and bearer": {h: hdr("apikey", k.ServiceRoleKey, "Authorization", "Bearer "+k.ServiceRoleKey)},
		"service_role apikey":            {h: hdr("apikey", k.ServiceRoleKey)},
		"service_role bearer only":       {h: hdr("Authorization", "Bearer "+k.ServiceRoleKey)},
		"publishable, legacy bearer":     {h: hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+k.ServiceRoleKey)},
		"secret key, legacy bearer":      {h: hdr("apikey", k.SecretKey, "Authorization", "Bearer "+k.ServiceRoleKey)},
		"anon apikey":                    {h: hdr("apikey", k.AnonKey)},
		"re-signed service_role apikey":  {h: hdr("apikey", resigned)},
		"service_role in the query":      {h: hdr(), query: "apikey=" + k.ServiceRoleKey},
	} {
		if res := authorize(st, k, testRef, c.h, c.query); res.status != http.StatusUnauthorized {
			t.Errorf("storage, %s: status %d, want 401", name, res.status)
		}
	}
	for name, h := range map[string]http.Header{
		"no credentials (public objects)": hdr(),
		"publishable":                     hdr("apikey", k.PublishableKey),
		"secret":                          hdr("apikey", k.SecretKey),
		"publishable, user session":       hdr("apikey", k.PublishableKey, "Authorization", "Bearer "+user),
		"user session only":               hdr("Authorization", "Bearer "+user),
		"temporary dashboard key":         hdr("apikey", tmp),
	} {
		if res := authorize(st, k, testRef, h, ""); res.status != 0 {
			t.Errorf("storage, %s: refused (%d %s)", name, res.status, res.body)
		}
	}
	// Enabled again, the legacy keys work on Storage as before.
	k.LegacyDisabled = false
	if res := authorize(st, k, testRef, hdr("apikey", k.ServiceRoleKey, "Authorization", "Bearer "+k.ServiceRoleKey), ""); res.status != 0 {
		t.Errorf("storage with legacy keys enabled: %d", res.status)
	}
}
