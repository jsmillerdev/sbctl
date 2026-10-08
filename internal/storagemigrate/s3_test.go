package storagemigrate

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

// fakeS3 is a bucket on an in-process S3 service. It does not keep Cache-Control, so the headers
// that arrive are recorded by a wrapper in front of it.
func fakeS3(t *testing.T) (*s3Bucket, *sentHeaders) {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("objects"); err != nil {
		t.Fatal(err)
	}
	sent := &sentHeaders{}
	fake := gofakes3.New(backend).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cc := r.Header.Get("Cache-Control"); cc != "" {
			sent.add(r.Method + " " + cc)
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	b, err := OpenS3(ctx, Destination{Bucket: "objects", Endpoint: srv.URL, Region: "us-east-1", PathStyle: true},
		Credentials{Source: CredFile, AccessKeyID: "test", SecretAccessKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return b.(*s3Bucket), sent
}

type sentHeaders struct {
	mu sync.Mutex
	h  []string
}

func (s *sentHeaders) add(v string) { s.mu.Lock(); s.h = append(s.h, v); s.mu.Unlock() }

func (s *sentHeaders) all() string { s.mu.Lock(); defer s.mu.Unlock(); return strings.Join(s.h, "|") }

func TestS3BucketRoundTrip(t *testing.T) {
	b, sent := fakeS3(t)
	body := []byte("hello bucket")
	keys := []string{"p/avatars/top.txt/v1", "p/avatars/sp ace/ü-é.txt/v2", "p/a+b=c.txt/v3", "q/other/v4"}
	for _, k := range keys {
		if err := b.Put(ctx, k, bytes.NewReader(body), int64(len(body)), FileMeta{ContentType: "text/plain", CacheControl: "max-age=60"}); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	var got []Entry
	if err := b.List(ctx, "p/", func(e Entry) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("list: %+v", got)
	}
	for _, e := range got {
		if e.Size != int64(len(body)) || time.Since(e.ModTime) > time.Hour {
			t.Errorf("entry %+v", e)
		}
	}
	rc, meta, err := b.Get(ctx, "p/avatars/sp ace/ü-é.txt/v2")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != string(body) || meta.ContentType != "text/plain" {
		t.Errorf("get: %q %+v", data, meta)
	}
	if !strings.Contains(sent.all(), "PUT max-age=60") {
		t.Errorf("Cache-Control sent with the uploads: %q", sent.all())
	}
	if err := b.Delete(ctx, "p/a+b=c.txt/v3", "p/never-was"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Get(ctx, "p/a+b=c.txt/v3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	if err := b.Put(ctx, "p/plain", bytes.NewReader(nil), 0, FileMeta{}); err != nil {
		t.Errorf("an empty object: %v", err)
	}
}

func TestS3BucketSendsLargeObjectsInParts(t *testing.T) {
	b, sent := fakeS3(t)
	b.threshold, b.partSize = 6<<20, 5<<20
	data := make([]byte, 11<<20+123)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "p/big/v1", bytes.NewReader(data), int64(len(data)), FileMeta{ContentType: "application/octet-stream", CacheControl: "no-store"}); err != nil {
		t.Fatal(err)
	}
	rc, meta, err := b.Get(ctx, "p/big/v1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Errorf("the object came back with %d bytes, want %d", len(got), len(data))
	}
	if meta.ContentType != "application/octet-stream" {
		t.Errorf("meta %+v", meta)
	}
	if !strings.Contains(sent.all(), "POST no-store") {
		t.Errorf("Cache-Control sent when the upload began: %q", sent.all())
	}
}

func TestOpenS3RefusesWhatCannotWork(t *testing.T) {
	cred := Credentials{Source: CredFile, AccessKeyID: "a", SecretAccessKey: "b"}
	for name, tc := range map[string]struct {
		d Destination
		c Credentials
		w string
	}{
		"no bucket":  {Destination{Region: "r"}, cred, "no bucket"},
		"no region":  {Destination{Bucket: "b"}, cred, "no region"},
		"half a key": {Destination{Bucket: "b", Region: "r"}, Credentials{Source: CredFile, AccessKeyID: "a"}, "incomplete"},
		"no role":    {Destination{Bucket: "b", Region: "r"}, Credentials{Source: CredRole}, "no role"},
		"unknown":    {Destination{Bucket: "b", Region: "r"}, Credentials{Source: "x"}, "unknown credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := OpenS3(ctx, tc.d, tc.c); err == nil || !strings.Contains(err.Error(), tc.w) {
				t.Errorf("OpenS3 = %v, want %q", err, tc.w)
			}
		})
	}
}

func TestRoleProviderAssumesTheRole(t *testing.T) {
	fake := awsfake.New(t)
	fake.SetEnv(t)
	const role = "arn:aws:iam::123456789012:role/storage"
	fake.AddRole(role)
	p, err := roleProvider(role)
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Retrieve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.AccessKeyID, "ASIAFAKEASSU") || c.SessionToken == "" || !c.CanExpire {
		t.Errorf("credentials: %+v", c)
	}
	if got := fake.Order("sts"); len(got) != 1 {
		t.Errorf("calls: %v", got)
	}
	bad, _ := roleProvider("arn:aws:iam::123456789012:role/unknown")
	if _, err := bad.Retrieve(ctx); err == nil || !strings.Contains(err.Error(), "assume arn:aws:iam::123456789012:role/unknown") {
		t.Errorf("an unregistered role: %v", err)
	}
}
