package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
	"github.com/supavise/supavise/internal/config"
)

const (
	testRoleARN = "arn:aws:iam::123456789012:role/supavise-storage"
	testToken   = "0123456789abcdef0123456789abcdef"
)

// stubSource is a credential provider whose answers the test scripts.
type stubSource struct {
	mu    sync.Mutex
	calls int
	creds awsapi.Credentials
	err   error
}

func (s *stubSource) Retrieve(context.Context) (awsapi.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.creds, s.err
}

func stubEndpoint(src awsapi.CredentialProvider) *StorageCredentials {
	return &StorageCredentials{token: testToken, arn: testRoleARN, src: src, log: discardLog(), now: time.Now}
}

func get(h http.Handler, remote, token, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:4010"+path, nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStorageCredentialsAnswerTheContainerShape(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	src := &stubSource{creds: awsapi.Credentials{AccessKeyID: "ASIAEXAMPLE", SecretAccessKey: "secret", SessionToken: "session", Expires: exp}}
	ep := stubEndpoint(src)
	rec := get(ep, "127.0.0.1:50000", testToken, http.MethodGet, "/credentials")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AccessKeyId": "ASIAEXAMPLE", "SecretAccessKey": "secret", "Token": "session",
		"Expiration": "2030-01-02T03:04:05Z", "RoleArn": testRoleARN,
	}
	if len(got) != len(want) {
		t.Errorf("fields = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// Only a loopback caller that presents the token gets anything, and a refusal says nothing.
func TestStorageCredentialsAnswerOnlyLoopbackCallersWithTheToken(t *testing.T) {
	src := &stubSource{creds: awsapi.Credentials{AccessKeyID: "ASIAEXAMPLE", SecretAccessKey: "secret", Expires: time.Now().Add(time.Hour)}}
	ep := stubEndpoint(src)
	for _, tc := range []struct {
		name, remote, token, method, path string
		want                              int
	}{
		{"no token", "127.0.0.1:1", "", "GET", "/credentials", 403},
		{"wrong token", "127.0.0.1:1", testToken + "x", "GET", "/credentials", 403},
		{"a prefix of the token", "127.0.0.1:1", testToken[:10], "GET", "/credentials", 403},
		{"the token with a scheme", "127.0.0.1:1", "Bearer " + testToken, "GET", "/credentials", 403},
		{"not loopback", "192.0.2.7:1", testToken, "GET", "/credentials", 403},
		{"not loopback, ipv6", "[2001:db8::1]:1", testToken, "GET", "/credentials", 403},
		{"no remote address", "", testToken, "GET", "/credentials", 403},
		{"ipv6 loopback", "[::1]:1", testToken, "GET", "/credentials", 200},
		{"another loopback address", "127.0.0.2:1", testToken, "GET", "/credentials", 200},
		{"another path", "127.0.0.1:1", testToken, "GET", "/", 404},
		{"another method", "127.0.0.1:1", testToken, "POST", "/credentials", 405},
		{"another method without the token", "127.0.0.1:1", "", "POST", "/credentials", 403},
	} {
		rec := get(ep, tc.remote, tc.token, tc.method, tc.path)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
		if tc.want == 403 && rec.Body.Len() != 0 {
			t.Errorf("%s: a refusal has a body: %q", tc.name, rec.Body.String())
		}
	}
	// A refusal never reaches the credentials: only the two requests that were answered asked.
	if src.calls != 2 {
		t.Errorf("the source was asked %d times; only the two answered requests may ask", src.calls)
	}
}

func TestStorageCredentialsFailureIsAGatewayErrorWithoutDetail(t *testing.T) {
	src := &stubSource{err: errors.New("AccessDenied: sts:AssumeRole on arn:aws:iam::123456789012:role/secret-detail")}
	ep := stubEndpoint(src)
	rec := get(ep, "127.0.0.1:1", testToken, http.MethodGet, "/credentials")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "AccessDenied") || strings.Contains(rec.Body.String(), "secret-detail") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// Through the real client: STS assumes the role once, the endpoint hands the same credentials to
// every request, and it assumes the role again when five minutes of its hour are left.
func TestStorageCredentialsAssumeTheRoleAndRefreshBeforeExpiry(t *testing.T) {
	fake := awsfake.New(t)
	fake.AddRole(testRoleARN)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	cfg := fake.Config()
	cfg.Now = clock
	client, err := awsapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n := newTestNode(t)
	n.cfg.Fleet.StorageS3RoleARN = testRoleARN
	ep, err := NewStorageCredentials(CredentialsOptions{Cfg: n.cfg, AWS: client, Token: testToken, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	read := func() map[string]string {
		t.Helper()
		rec := get(ep, "127.0.0.1:1", testToken, http.MethodGet, "/credentials")
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var m map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	assumes := func() int {
		c := 0
		for _, o := range fake.Order("sts") {
			if o == "sts:AssumeRole" {
				c++
			}
		}
		return c
	}
	first := read()
	if !strings.HasPrefix(first["AccessKeyId"], "ASIAFAKEASSUMED") || first["RoleArn"] != testRoleARN {
		t.Fatalf("first = %v", first)
	}
	if exp, err := time.Parse(time.RFC3339, first["Expiration"]); err != nil || exp.Before(now.Add(50*time.Minute)) {
		t.Fatalf("expiration %q", first["Expiration"])
	}
	for _, c := range fake.Calls() {
		if c.Action == "AssumeRole" && (c.Params.Get("RoleArn") != testRoleARN || c.Params.Get("RoleSessionName") != storageSessionName || c.Params.Get("DurationSeconds") != "3600") {
			t.Errorf("AssumeRole params = %v", c.Params)
		}
	}
	for range 3 {
		if got := read(); got["AccessKeyId"] != first["AccessKeyId"] {
			t.Fatal("the credentials changed within their hour")
		}
	}
	if assumes() != 1 {
		t.Fatalf("%d AssumeRole calls for four requests", assumes())
	}
	mu.Lock()
	now = now.Add(54 * time.Minute) // still more than five minutes left
	mu.Unlock()
	if read()["AccessKeyId"] != first["AccessKeyId"] || assumes() != 1 {
		t.Fatal("refreshed too early")
	}
	mu.Lock()
	now = now.Add(2 * time.Minute) // under five minutes left
	mu.Unlock()
	if second := read(); second["AccessKeyId"] == first["AccessKeyId"] || assumes() != 2 {
		t.Fatalf("not refreshed with five minutes left: %v, %d calls", second, assumes())
	}
}

func TestNewStorageCredentialsNeedsARole(t *testing.T) {
	n := newTestNode(t)
	if _, err := NewStorageCredentials(CredentialsOptions{Cfg: n.cfg, Token: testToken}); err == nil {
		t.Fatal("an endpoint without a role")
	}
}

func TestStorageCredentialsServeOverTCP(t *testing.T) {
	src := &stubSource{creds: awsapi.Credentials{AccessKeyID: "ASIAEXAMPLE", SecretAccessKey: "secret", Expires: time.Now().Add(time.Hour)}}
	ep := stubEndpoint(src)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ctx, ln) }()
	url := "http://" + ln.Addr().String() + "/credentials"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "ASIAEXAMPLE") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	req, _ = http.NewRequest("GET", url, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("without the token: %v %v", resp, err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return when its context ended")
	}
}

func TestStorageCredentialTokenIsPerBootAndPrivate(t *testing.T) {
	n := newTestNode(t)
	boot := "boot-1"
	old := bootID
	bootID = func() string { return boot }
	t.Cleanup(func() { bootID = old })

	a, err := StorageCredentialToken(n.cfg)
	if err != nil || len(a) != 64 {
		t.Fatalf("token %q, %v", a, err)
	}
	fi, err := os.Stat(credentialTokenPath(n.cfg))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v", fi, err)
	}
	// The daemon restarts, or a CLI renders Storage again: the token Storage runs with stays.
	if b, err := StorageCredentialToken(n.cfg); err != nil || b != a {
		t.Fatalf("same boot: %q vs %q, %v", b, a, err)
	}
	// A reboot replaces it.
	boot = "boot-2"
	c, err := StorageCredentialToken(n.cfg)
	if err != nil || c == a || len(c) != 64 {
		t.Fatalf("new boot: %q vs %q, %v", c, a, err)
	}
	// A file that was loosened or damaged is made right.
	if err := os.Chmod(credentialTokenPath(n.cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialTokenPath(n.cfg), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := StorageCredentialToken(n.cfg)
	if err != nil || len(d) != 64 {
		t.Fatalf("damaged file: %q, %v", d, err)
	}
	if fi, _ := os.Stat(credentialTokenPath(n.cfg)); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
}

// Two processes that render Storage's environment and the daemon that checks the token start
// together at boot: they must end with one token.
func TestStorageCredentialTokenIsOneTokenUnderRaces(t *testing.T) {
	n := newTestNode(t)
	old := bootID
	bootID = func() string { return "boot" }
	t.Cleanup(func() { bootID = old })
	const k = 12
	got := make([]string, k)
	var wg sync.WaitGroup
	for i := range k {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = StorageCredentialToken(n.cfg)
		}()
	}
	wg.Wait()
	for _, g := range got {
		if g == "" || g != got[0] {
			t.Fatalf("tokens = %v", got)
		}
	}
}

func s3Config(n *testNode) {
	n.cfg.Supervisor = config.SupervisorSystemd
	n.cfg.Fleet.StorageBackend = "s3"
	n.cfg.Fleet.StorageS3Bucket = "objects"
}

func TestStorageEnvWithARoleHasNoKey(t *testing.T) {
	n := newTestNode(t)
	s3Config(n)
	n.cfg.Fleet.StorageS3RoleARN = testRoleARN
	n.cfg.Fleet.StorageCredentialsPort = 4123
	c, err := loadCreds(context.Background(), n.deps(), true)
	if err != nil {
		t.Fatal(err)
	}
	env, err := storageEnv(n.cfg, c, testToken)
	if err != nil {
		t.Fatalf("a role under systemd: %v", err)
	}
	if env["AWS_CONTAINER_CREDENTIALS_FULL_URI"] != "http://127.0.0.1:4123/credentials" || env["AWS_CONTAINER_AUTHORIZATION_TOKEN"] != testToken {
		t.Errorf("env = %v", env)
	}
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s is set", k)
		}
	}
	if env["STORAGE_BACKEND"] != "s3" || env["STORAGE_S3_BUCKET"] != "objects" {
		t.Errorf("env = %v", env)
	}
	if _, err := storageEnv(n.cfg, c, ""); err == nil {
		t.Error("a role without a token")
	}
	// The static keys are the other way, and name the role in the refusal.
	n.cfg.Fleet.StorageS3RoleARN = ""
	if _, err := storageEnv(n.cfg, c, ""); err == nil || !strings.Contains(err.Error(), "storage_s3_role_arn") {
		t.Errorf("s3 under systemd with neither = %v", err)
	}
	// The role is for S3 only.
	n.cfg.Fleet.StorageBackend, n.cfg.Fleet.StorageS3RoleARN = "file", testRoleARN
	env, err = storageEnv(n.cfg, c, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["AWS_CONTAINER_CREDENTIALS_FULL_URI"]; ok {
		t.Error("the file backend got credentials")
	}
}

// The unit Storage runs from carries the token of this boot, so a render by a CLI and a render by
// the daemon agree and restart nothing.
func TestStorageSpecCarriesTheTokenOfThisBoot(t *testing.T) {
	r := newManagerRig(t, func(d *Deps) {
		d.Cfg.Supervisor = config.SupervisorSystemd
		d.Cfg.Fleet.StorageBackend = "s3"
		d.Cfg.Fleet.StorageS3Bucket = "objects"
		d.Cfg.Fleet.StorageS3RoleARN = testRoleARN
	})
	ctx := context.Background()
	if err := r.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tok, err := StorageCredentialToken(r.n.cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := r.sup.specs["supavise-storage.service"].Env
	if env["AWS_CONTAINER_AUTHORIZATION_TOKEN"] != tok || env["AWS_CONTAINER_CREDENTIALS_FULL_URI"] == "" {
		t.Fatalf("env = %v", env)
	}
	again, err := r.m.Specs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range again {
		if s.Service == config.SvcStorage && s.Env["AWS_CONTAINER_AUTHORIZATION_TOKEN"] != tok {
			t.Error("a second render has another token")
		}
	}
	if strings.Contains(strings.Join(flattenEnv(env), "\n"), "AKIA") {
		t.Error("a key in the environment")
	}
}

func flattenEnv(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
