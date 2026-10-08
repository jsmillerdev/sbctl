package awsapi_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

var testCreds = awsapi.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

// testNow is the clock of the golden tests.
var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// answer is a canned reply.
type answer struct {
	status int
	body   string
	header map[string]string
}

func xmlOK(body string) answer { return answer{status: 200, body: body} }

// request is what the stub saw.
type request struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// stub answers requests with the canned replies in order (the last one repeats), records them,
// and fails the test when a signature does not verify. It returns a Config that points every
// service at it and signs with testCreds at testNow.
func stub(t *testing.T, answers ...answer) (awsapi.Config, func() []request) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, err := awsfake.Verify(r, body, func(id string) (awsapi.Credentials, bool) { return testCreds, id == testCreds.AccessKeyID }); err != nil {
			t.Errorf("signature: %v", err)
		}
		mu.Lock()
		n := len(seen)
		seen = append(seen, request{r.Method, r.URL.Path, r.Header.Clone(), string(body)})
		mu.Unlock()
		a := answers[min(n, len(answers)-1)]
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	cfg := awsapi.Config{
		Region:       "us-east-1",
		Credentials:  awsapi.StaticCredentials(testCreds),
		Endpoints:    awsapi.Endpoints{EC2: srv.URL, SecretsManager: srv.URL, STS: srv.URL, IMDS: srv.URL},
		Getenv:       func(string) string { return "" },
		Now:          func() time.Time { return testNow },
		RetryBackoff: time.Millisecond,
	}
	return cfg, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), seen...)
	}
}

func newClient(t *testing.T, cfg awsapi.Config) *awsapi.Client {
	t.Helper()
	c, err := awsapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func asError(err error, target **awsapi.Error) bool { return errors.As(err, target) }
