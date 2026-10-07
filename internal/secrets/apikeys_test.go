package secrets

import (
	"testing"
	"time"
)

func TestOpaqueKeysFromStoredRecords(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	k, err := NewProjectKeys(ref, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m := k.Map()
	if got := KeysFromMap(m).OpaqueKeys(ref); len(got) != 2 || got[0].Type != KeyTypePublishable || got[1].Type != KeyTypeSecret {
		t.Fatalf("a project with no records has its two default keys, got %+v", got)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	extra := APIKeyRecord{ID: "b", Name: "ci", Type: KeyTypeSecret, Key: NewSecretKey(), CreatedAt: t0}
	older := APIKeyRecord{ID: "a", Name: "web", Type: KeyTypePublishable, Key: NewPublishableKey(), CreatedAt: t0.Add(-time.Hour)}
	for _, r := range []APIKeyRecord{extra, older} {
		b, _ := MarshalRecord(r)
		m[RecordSecretName(r.ID)] = string(b)
	}
	revoked := APIKeyRecord{ID: KeyID(ref, DefaultKeyName, KeyTypeSecret), Name: DefaultKeyName, Type: KeyTypeSecret, Default: true, Revoked: true}
	b, _ := MarshalRecord(revoked)
	m[RecordSecretName(revoked.ID)] = string(b)
	m[NameLegacyKeys] = string(MarshalLegacyState(false))
	m[NamePrefixAPIKey+"broken"] = "{not json"

	got := KeysFromMap(m)
	if !got.LegacyDisabled {
		t.Error("legacy switch not read")
	}
	keys := got.OpaqueKeys(ref)
	if len(keys) != 3 {
		t.Fatalf("want default publishable, two extras and no revoked default secret, got %+v", keys)
	}
	if keys[0].Key != k.PublishableKey || keys[1].Key != older.Key || keys[2].Key != extra.Key {
		t.Errorf("order or values wrong: %+v", keys)
	}
	for _, o := range keys {
		if o.Key == k.SecretKey {
			t.Error("a revoked default key is still listed")
		}
	}
	if len(got.AllRecords()) != 3 {
		t.Errorf("AllRecords lists the revoked record too, got %d", len(got.AllRecords()))
	}
	if _, ok := got.Record(revoked.ID); !ok {
		t.Error("Record lost the tombstone")
	}
}

func TestReviveDefaultsLeavesTheCopyAlone(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	k := &ProjectKeys{PublishableKey: NewPublishableKey(), SecretKey: NewSecretKey()}
	pubID, secID := KeyID(ref, DefaultKeyName, KeyTypePublishable), KeyID(ref, DefaultKeyName, KeyTypeSecret)
	k.SetRecord(APIKeyRecord{ID: secID, Name: DefaultKeyName, Type: KeyTypeSecret, Default: true, Revoked: true})
	k.SetRecord(APIKeyRecord{ID: pubID, Name: "renamed", Type: KeyTypePublishable, Default: true})
	k.SetRecord(APIKeyRecord{ID: "other", Name: "ci", Type: KeyTypeSecret, Key: NewSecretKey(), Revoked: true})
	before := *k
	nk := before
	got := nk.ReviveDefaults(ref, time.Unix(100, 0))
	if len(got) != 1 || got[0].ID != secID || got[0].Revoked {
		t.Fatalf("revived: %+v", got)
	}
	if r, _ := nk.Record(secID); r.Revoked {
		t.Fatal("the secret default is still revoked")
	}
	if r, _ := nk.Record("other"); !r.Revoked {
		t.Fatal("only the defaults are revived")
	}
	if r, _ := before.Record(secID); !r.Revoked {
		t.Fatal("the copy taken before must keep its records")
	}
	if n := len(nk.OpaqueKeys(ref)); n != 2 {
		t.Fatalf("active keys: %d", n)
	}
	var none ProjectKeys
	if none.ReviveDefaults(ref, time.Now()) != nil {
		t.Fatal("nothing to revive")
	}
}

func TestDeriveIsBoundToTheMasterKeyAndTheLabel(t *testing.T) {
	a, _ := New(make([]byte, 32))
	b, _ := New([]byte("0123456789abcdef0123456789abcdef"))
	if string(a.Derive("x")) != string(a.Derive("x")) || string(a.Derive("x")) == string(a.Derive("y")) || string(a.Derive("x")) == string(b.Derive("x")) || len(a.Derive("x")) != 32 {
		t.Fatal("Derive must be deterministic, per label and per key")
	}
}
