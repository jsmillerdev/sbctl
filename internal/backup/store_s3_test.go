package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// Environment of the real-S3 variant (CI runs it against a MinIO service container;
// any S3-compatible endpoint works). All of the first four are required to enable it.
const (
	envS3Endpoint  = "SBCTL_TEST_S3_ENDPOINT"
	envS3Bucket    = "SBCTL_TEST_S3_BUCKET"
	envS3AccessKey = "SBCTL_TEST_S3_ACCESS_KEY"
	envS3SecretKey = "SBCTL_TEST_S3_SECRET_KEY"
	envS3Region    = "SBCTL_TEST_S3_REGION" // optional, default us-east-1
)

// fakeS3 starts an in-process S3 server (gofakes3) with one bucket and returns options
// that point an S3Store at it. This verifies the S3 code path without Docker; it is
// not a substitute for the real-service run in CI (see s3FromEnv).
func fakeS3(t *testing.T) S3Options {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("sbctl-test"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	return S3Options{Bucket: "sbctl-test", Prefix: "backups/node1", Endpoint: srv.URL, Region: "us-east-1",
		ForcePathStyle: true, AccessKeyID: "test", SecretKey: "test", PartSize: 5 << 20}
}

// s3FromEnv returns options for the real service named by SBCTL_TEST_S3_*, with a
// unique prefix so concurrent runs do not collide, or skips.
func s3FromEnv(t *testing.T) S3Options {
	t.Helper()
	ep, bucket, ak, sk := os.Getenv(envS3Endpoint), os.Getenv(envS3Bucket), os.Getenv(envS3AccessKey), os.Getenv(envS3SecretKey)
	if ep == "" || bucket == "" || ak == "" || sk == "" {
		t.Skipf("real S3 test disabled: set %s, %s, %s and %s", envS3Endpoint, envS3Bucket, envS3AccessKey, envS3SecretKey)
	}
	region := os.Getenv(envS3Region)
	if region == "" {
		region = "us-east-1"
	}
	return S3Options{Bucket: bucket, Prefix: fmt.Sprintf("sbctl-test/%d-%d", time.Now().Unix(), os.Getpid()), Endpoint: ep,
		Region: region, ForcePathStyle: true, AccessKeyID: ak, SecretKey: sk, PartSize: 5 << 20}
}

// cleanS3 deletes everything under the store's prefix when the test ends.
func cleanS3(t *testing.T, st *S3Store) {
	t.Cleanup(func() {
		objs, err := st.List(context.Background(), "")
		if err != nil {
			return
		}
		keys := make([]string, len(objs))
		for i, o := range objs {
			keys[i] = o.Key
		}
		_ = st.Delete(context.Background(), keys...)
	})
}

func runS3Suite(t *testing.T, opts S3Options) {
	st, err := NewS3Store(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	cleanS3(t, st)

	t.Run("contract", func(t *testing.T) { storeContract(t, st) })

	t.Run("multipart", func(t *testing.T) {
		big := make([]byte, 12<<20+12345) // three parts of 5 MiB, the last short
		rand.Read(big)
		key := "mp/big.bin"
		if err := st.Put(context.Background(), key, bytes.NewReader(big)); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, st, key); !bytes.Equal(got, big) {
			t.Fatalf("multipart object differs (%d vs %d bytes)", len(got), len(big))
		}
		// Exactly one part boundary: must not leave an empty trailing part.
		exact := make([]byte, 10<<20)
		rand.Read(exact)
		if err := st.Put(context.Background(), "mp/exact.bin", bytes.NewReader(exact)); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, st, "mp/exact.bin"); !bytes.Equal(got, exact) {
			t.Fatal("object of exactly two parts differs")
		}
		// A failing source aborts the upload and leaves no object.
		err := st.Put(context.Background(), "mp/broken.bin", io.MultiReader(bytes.NewReader(big[:6<<20]), errReader{fmt.Errorf("source failed")}))
		if err == nil {
			t.Fatal("Put with a failing source succeeded")
		}
		if _, err := st.Stat(context.Background(), "mp/broken.bin"); err == nil {
			t.Fatal("aborted multipart upload became visible")
		}
	})

	t.Run("many deletes", func(t *testing.T) {
		ctx := context.Background()
		var keys []string
		for i := 0; i < 1100; i++ { // more than one DeleteObjects batch
			k := fmt.Sprintf("bulk/%04d", i)
			keys = append(keys, k)
			if i%50 == 0 { // keep uploads few; Delete of missing keys must be fine
				if err := st.Put(ctx, k, strings.NewReader("x")); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := st.Delete(ctx, keys...); err != nil {
			t.Fatal(err)
		}
		if objs, _ := st.List(ctx, "bulk/"); len(objs) != 0 {
			t.Fatalf("%d objects left", len(objs))
		}
	})

	t.Run("wal through Service", func(t *testing.T) {
		e := newTestEnv(t)
		e.svc.opt.Store, e.store = st, nil
		ctx := context.Background()
		name := walName(1, 3)
		src := writeFile(t, e.root+"/w/"+name, bytes.Repeat([]byte("s3 wal "), 50000))
		if err := e.svc.PushWAL(ctx, testRef, src); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.PushWAL(ctx, testRef, src); err != nil {
			t.Fatalf("identical re-push: %v", err)
		}
		other := writeFile(t, e.root+"/o/"+name, []byte("different"))
		if err := e.svc.PushWAL(ctx, testRef, other); err == nil || !strings.Contains(err.Error(), "different content") {
			t.Fatalf("different content = %v", err)
		}
		dest := e.root + "/fetched"
		if err := e.svc.FetchWAL(ctx, testRef, name, dest); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(dest); len(b) != 7*50000 {
			t.Fatalf("fetched %d bytes", len(b))
		}
		if err := e.svc.FetchWAL(ctx, testRef, walName(1, 4), e.root+"/nope"); err == nil || !strings.Contains(err.Error(), ErrNoWAL.Error()) {
			t.Fatalf("missing = %v", err)
		}
	})
}

func TestS3StoreAgainstFakeServer(t *testing.T) { runS3Suite(t, fakeS3(t)) }

// TestS3StoreAgainstRealService is the CI-only variant: it needs a real S3-compatible
// endpoint (a MinIO service container in CI) and is skipped without SBCTL_TEST_S3_*.
func TestS3StoreAgainstRealService(t *testing.T) { runS3Suite(t, s3FromEnv(t)) }

// A 403 on a key lookup is what S3 answers for a missing key when the caller lacks
// s3:ListBucket; the error must say so instead of looking like a plain failure.
func TestAccessDeniedHintNamesListBucket(t *testing.T) {
	forbidden := &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusForbidden}},
		Err:      errors.New("AccessDenied"),
	}}
	if h := accessDeniedHint(forbidden); !strings.Contains(h, "s3:ListBucket") {
		t.Errorf("hint = %q", h)
	}
	if isNotFound(forbidden) {
		t.Error("a 403 must not be taken for a missing key")
	}
	if h := accessDeniedHint(errors.New("boom")); h != "" {
		t.Errorf("hint for an unrelated error = %q", h)
	}
}
