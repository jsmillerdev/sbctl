package storagemigrate

import (
	"bytes"
	"context"
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

// virtualLimiter is a limiter whose clock moves only when it sleeps, so a test does not wait.
func virtualLimiter(rate float64) *limiter {
	var mu sync.Mutex
	now := time.Unix(0, 0)
	return &limiter{rate: rate,
		now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
			return nil
		}}
}

// zeroAt is size bytes of zeros that take no memory.
type zeroAt struct{}

func (zeroAt) ReadAt(p []byte, _ int64) (int, error) { clear(p); return len(p), nil }

func TestThePacedTransportChargesEveryByteOnce(t *testing.T) {
	b, _ := fakeS3(t)
	// Over plain http the SDK reads a body twice, once to sign it and once to send it; only the second
	// is on the wire.
	l := virtualLimiter(1 << 20)
	b.pace(l)
	const size = 3 << 20
	if err := b.Put(ctx, "p/single/v1", zeroAt{}, size, FileMeta{}); err != nil {
		t.Fatal(err)
	}
	if l.bytes != size {
		t.Errorf("one request: %d bytes were paid for, want %d", l.bytes, size)
	}
	b.threshold, b.partSize = 1<<20, 1<<20
	if err := b.Put(ctx, "p/parts/v1", zeroAt{}, size, FileMeta{}); err != nil {
		t.Fatal(err)
	}
	// The parts and the few hundred bytes of the request that completes the upload.
	if extra := l.bytes - 2*size; extra < 0 || extra > 1024 {
		t.Errorf("in parts: %d bytes were paid for, want %d", l.bytes-size, size)
	}
	paid := l.bytes
	// An empty object costs nothing but its headers.
	before := l.bytes
	if err := b.Put(ctx, "p/empty/v1", zeroAt{}, 0, FileMeta{}); err != nil {
		t.Fatal(err)
	}
	if l.bytes != before {
		t.Errorf("an empty object: %d bytes were paid for", l.bytes-before)
	}
	// Without a rate nothing is paid for.
	l.setRate(0)
	if err := b.Put(ctx, "p/free/v1", zeroAt{}, size, FileMeta{}); err != nil {
		t.Fatal(err)
	}
	if l.bytes != paid {
		t.Errorf("without a limit %d bytes were paid for", l.bytes-paid)
	}
	rc, _, err := b.Get(ctx, "p/single/v1")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, rc); err != nil || n != size {
		t.Errorf("Get: %d bytes, %v", n, err)
	}
	rc.Close()
}

func TestAStalledConnectionEndsTheRequest(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut: // takes no data and answers nothing
			<-release
		case http.MethodGet: // sends a little and goes quiet
			w.Header().Set("Content-Length", "1000000")
			_, _ = w.Write(make([]byte, 100))
			w.(http.Flusher).Flush()
			<-release
		}
	}))
	t.Cleanup(func() { close(release); srv.CloseClientConnections(); srv.Close() })
	bk, err := openS3(ctx, Destination{Bucket: "objects", Endpoint: srv.URL, Region: "us-east-1", PathStyle: true},
		Credentials{Source: CredFile, AccessKeyID: "test", SecretAccessKey: "test"}, 300*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := bk.(*s3Bucket)

	start := time.Now()
	if err := b.Put(ctx, "p/x/v1", zeroAt{}, 48<<20, FileMeta{}); !errors.Is(err, errStalled) {
		t.Errorf("Put = %v", err)
	}
	rc, _, err := b.Get(ctx, "p/x/v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n, err := io.Copy(io.Discard, rc); !errors.Is(err, errStalled) || n != 100 {
		t.Errorf("reading the body: %d bytes, %v", n, err)
	}
	rc.Close()
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("the requests took %s to end", d)
	}
}

func TestPlainRemote(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"": false, "https://s3.example.test": false, "http://127.0.0.1:9000": false, "http://localhost:3900": false,
		"http://[::1]:9000": false, "http://minio.localhost:9000": false,
		"http://10.0.0.5:9000": true, "http://s3.test": true, "http://s3.example.test:9000": true,
	} {
		if got := plainRemote(endpoint); got != want {
			t.Errorf("plainRemote(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

// A store that assembles a large object's parts before it answers CompleteMultipartUpload may take
// longer than a connection that moves no data is believed to be alive. That request gets its own,
// longer wait for the answer once its body is sent, and is not ended and started over.
func TestTheAnswerToCompleteMultipartUploadMayTakeLongerThanAStall(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("objects"); err != nil {
		t.Fatal(err)
	}
	fake := gofakes3.New(backend).Server()
	var slow, completes int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if completesMultipart(r) {
			mu.Lock()
			completes++
			mu.Unlock()
			time.Sleep(700 * time.Millisecond) // longer than the stall limit below
			mu.Lock()
			slow++
			mu.Unlock()
		}
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	bk, err := openS3(ctx, Destination{Bucket: "objects", Endpoint: srv.URL, Region: "us-east-1", PathStyle: true},
		Credentials{Source: CredFile, AccessKeyID: "test", SecretAccessKey: "test"}, 300*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	b := bk.(*s3Bucket)
	b.tr.complete = 10 * time.Second
	b.threshold, b.partSize = 6<<20, 5<<20
	data := make([]byte, 11<<20)
	if err := b.Put(ctx, "p/big/v1", bytes.NewReader(data), int64(len(data)), FileMeta{}); err != nil {
		t.Fatalf("Put = %v (the answer to the completion took longer than a stall but is not one)", err)
	}
	if completes != 1 || slow != 1 {
		t.Fatalf("%d completions, %d answered", completes, slow)
	}

	// Every other request keeps the stall limit.
	release := make(chan struct{})
	quiet := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); quiet.CloseClientConnections(); quiet.Close() })
	bk, err = openS3(ctx, Destination{Bucket: "objects", Endpoint: quiet.URL, Region: "us-east-1", PathStyle: true},
		Credentials{Source: CredFile, AccessKeyID: "test", SecretAccessKey: "test"}, 300*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	bk.(*s3Bucket).tr.complete = 10 * time.Second
	if err := bk.Put(ctx, "p/x/v1", bytes.NewReader([]byte("x")), 1, FileMeta{}); !errors.Is(err, errStalled) {
		t.Errorf("a plain Put to a store that never answers = %v", err)
	}
}

// A key is a file name, and a file name can hold an escape sequence: the errors that carry one
// show it quoted, as the rest of the run does.
func TestErrorsQuoteTheKeysTheyName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) }))
	t.Cleanup(srv.Close)
	bk, err := openS3(ctx, Destination{Bucket: "objects", Endpoint: srv.URL, Region: "us-east-1", PathStyle: true},
		Credentials{Source: CredFile, AccessKeyID: "test", SecretAccessKey: "test"}, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	key := "p/evil\x1b[31m\nname/v1"
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error", what)
			return
		}
		if strings.ContainsAny(err.Error(), "\x1b\n") {
			t.Errorf("%s: the error holds a raw control character: %q", what, err.Error())
		}
	}
	check("put", bk.Put(ctx, key, bytes.NewReader([]byte("x")), 1, FileMeta{}))
	_, _, err = bk.Get(ctx, key)
	check("get", err)
	check("list", bk.List(ctx, key, func(Entry) error { return nil }))
	check("delete", bk.Delete(ctx, key))
	b := bk.(*s3Bucket)
	b.threshold, b.partSize = 1, 1
	check("multipart", b.Put(ctx, key, bytes.NewReader([]byte("xx")), 2, FileMeta{}))
}
