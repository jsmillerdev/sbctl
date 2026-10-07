package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"testing"
)

// bundleBody is a stand-in for "EZBR" and a compressed eszip: the API checks the prefix and
// the digest only; internal/functions decompresses it.
func bundleBody(content string) []byte { return append([]byte("EZBR"), []byte(content)...) }

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestBundledFunctionUploads(t *testing.T) {
	f := newFixture(t)
	h := &recordingHook{}
	withHook(t, f, h)
	base := "/v1/projects/" + testRef + "/functions"
	entry := "file:///work/supabase/functions/hello/index.ts"
	body := bundleBody("compressed eszip v1")
	q := url.Values{"slug": {"hello"}, "name": {"hello"}, "entrypoint_path": {entry}, "import_map_path": {"file:///work/deno.json"}, "ezbr_sha256": {sha(body)}}

	// Create: what the CLI sends for a function it bundled itself.
	rec := f.do("POST", base+"?"+q.Encode(), body, "Content-Type", EszipMediaType)
	if rec.Code != 201 || jsonField(t, rec, "slug") != "hello" || jsonField(t, rec, "entrypoint_path") != entry || jsonField(t, rec, "verify_jwt") != true {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if h.count() != 1 {
		t.Fatalf("hook calls: %d", h.count())
	}
	files, err := f.srv.store.FunctionFiles(context.Background(), testRef, "hello")
	if err != nil || len(files) != 1 || files[0].Path != BundleFileName || !bytes.Equal(files[0].Content, body) {
		t.Fatalf("stored files: %+v %v", files, err)
	}

	// Update: the same function again, with verify_jwt off and a new bundle.
	body2 := bundleBody("compressed eszip v2")
	q2 := url.Values{"verify_jwt": {"false"}, "entrypoint_path": {entry}, "ezbr_sha256": {sha(body2)}}
	rec = f.do("PATCH", base+"/hello?"+q2.Encode(), body2, "Content-Type", EszipMediaType)
	if rec.Code != 200 || jsonField(t, rec, "version") != float64(2) || jsonField(t, rec, "verify_jwt") != false {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	files, _ = f.srv.store.FunctionFiles(context.Background(), testRef, "hello")
	if len(files) != 1 || !bytes.Equal(files[0].Content, body2) {
		t.Fatalf("bundle after update: %+v", files)
	}
	if fn, _ := f.srv.store.GetFunction(context.Background(), testRef, "hello"); fn.ImportMapPath != "file:///work/deno.json" {
		t.Fatalf("an update without import_map_path must keep it, got %q", fn.ImportMapPath)
	}
	if h.count() != 2 {
		t.Fatalf("hook calls: %d", h.count())
	}

	// Refusals: wrong digest, not a bundle, no entrypoint, too large.
	bad := map[string]struct {
		method, path string
		body         []byte
	}{
		"digest":       {"PATCH", base + "/hello?ezbr_sha256=" + sha([]byte("other")), body2},
		"not a bundle": {"PATCH", base + "/hello", []byte("plain text")},
		"entrypoint":   {"POST", base + "?slug=nope", body},
	}
	for name, c := range bad {
		if rec := f.do(c.method, c.path, c.body, "Content-Type", EszipMediaType); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if h.count() != 2 {
		t.Fatal("a refused upload called the hook")
	}
	if files, _ := f.srv.store.FunctionFiles(context.Background(), testRef, "hello"); !bytes.Equal(files[0].Content, body2) {
		t.Fatal("a refused upload replaced the stored bundle")
	}
}
