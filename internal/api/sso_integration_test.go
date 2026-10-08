package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
)

// itArts serves unpacked artifact directories; Tag is only a label.
type itArts map[string]string

func (a itArts) Dir(svc string) (string, error) {
	if d, ok := a[svc]; ok {
		return d, nil
	}
	return "", fmt.Errorf("no artifact for %s", svc)
}
func (a itArts) Tag(svc string) (string, error) { return filepath.Base(a[svc]), nil }

var inputRe = regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*value="([^"]*)"`)
var formActionRe = regexp.MustCompile(`<form[^>]*action="([^"]*)"`)

// TestIntegrationDashboardSSO runs the dashboard's single sign-on end to end on this machine,
// with the real supavise-gotrue@system (SAML on, the before-user-created hook pointing at this
// process's Management API) and a SAML identity provider written for tests
// (testdata/saml-idp.py): a person of an allowed domain signs in and gets the domain's default
// role, a person of any other domain is let through GoTrue and refused by the API until an
// administrator approves them, and nobody can sign up any other way.
//
//	SUPAVISE_SSO_INTEGRATION=1 SUPAVISE_TEST_UNPACKED=$HOME/.cache/sbctl/unpacked SUPAVISE_SSO_PYTHON=/path/to/python-with-signxml \
//	  SUPAVISE_API_IT_PORT_BASE=44100 scripts/guard.sh -- go test ./internal/api -run IntegrationDashboardSSO -v
//
// It starts a PostgreSQL cluster, GoTrue and a Python process, and stops them again.
func TestIntegrationDashboardSSO(t *testing.T) {
	if os.Getenv("SUPAVISE_SSO_INTEGRATION") == "" {
		t.Skip("SUPAVISE_SSO_INTEGRATION not set")
	}
	root, python := os.Getenv("SUPAVISE_TEST_UNPACKED"), os.Getenv("SUPAVISE_SSO_PYTHON")
	if root == "" || python == "" {
		t.Skip("SUPAVISE_TEST_UNPACKED and SUPAVISE_SSO_PYTHON are needed")
	}
	arts := itArts{}
	for svc, glob := range map[string]string{config.SvcPostgres: "postgres-17*", config.SvcGoTrue: "auth-*", config.SvcPostgREST: "postgrest-*"} {
		m, _ := filepath.Glob(filepath.Join(root, glob))
		if len(m) == 0 {
			t.Skipf("no %s artifact under %s", svc, root)
		}
		arts[svc] = m[len(m)-1]
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1)")
	}
	ports := distinctFreePorts(t, 5)
	pgPort, gtPort, adminPort, idpPort, idp2Port := ports[0], ports[1], ports[2], ports[3], ports[4]

	cfg := config.Default()
	cfg.StateDir, _ = os.MkdirTemp("/tmp", "sbs")
	t.Cleanup(func() { os.RemoveAll(cfg.StateDir) })
	cfg.KeyPath = filepath.Join(cfg.StateDir, "master.key")
	cfg.Supervisor = config.SupervisorExec
	cfg.Domain = "supavise.test"
	cfg.TLS.Mode = "off"
	cfg.BinPath = truePath
	cfg.Ports.SystemPostgres, cfg.Ports.SystemGoTrue, cfg.Ports.ProjectBase = pgPort, gtPort, pgPort+2
	cfg.Listen.Admin = fmt.Sprintf("127.0.0.1:%d", adminPort)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	oo := lifecycle.OpenOptions{Artifacts: arts}
	t.Cleanup(func() {
		if err := lifecycle.StopAll(context.Background(), cfg, oo); err != nil {
			t.Errorf("StopAll: %v", err)
		}
	})
	node, err := lifecycle.InitSystem(ctx, cfg, oo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()

	srv, err := NewServer(Deps{Registry: node.Registry, Secrets: node.Secrets, Manager: node.Engine, Config: cfg, Settings: node.Settings,
		Logger: itLogger()})
	if err != nil {
		t.Fatal(err)
	}
	serve := func() *http.Server {
		l, err := net.Listen("tcp", cfg.Listen.Admin)
		if err != nil {
			t.Fatal(err)
		}
		hs := &http.Server{Handler: srv}
		go hs.Serve(l)
		return hs
	}
	admin := serve()
	t.Cleanup(func() { admin.Close() })

	startIdP := func(port int, name string) {
		cmd := exec.Command(python, "testdata/saml-idp.py", fmt.Sprint(port), name)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		line, _ := bufio.NewReader(out).ReadString('\n')
		if !strings.HasPrefix(line, "ready") {
			t.Fatalf("the test identity provider did not start: %q", line)
		}
	}
	startIdP(idpPort, "idp")
	startIdP(idp2Port, "idp2")

	hc := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	gt := fmt.Sprintf("http://127.0.0.1:%d", gtPort)
	adm := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	call := func(method, u, token string, body any) (int, []byte, http.Header) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequestWithContext(ctx, method, u, rd)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, u, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	field := func(b []byte, path ...string) any {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[p]
		}
		return v
	}

	// The first administrator, as the installer's claim token makes one.
	tok, _, err := srv.accounts.IssueClaimToken(ctx, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if st, b, _ := call("POST", adm+"/claim", "", map[string]any{"token": tok, "email": "owner@acme.test", "password": "correct-horse-battery-staple", "organization_name": "Acme"}); st != 201 {
		t.Fatalf("claim: %d %s", st, b)
	}
	st, b, _ := call("POST", gt+"/token?grant_type=password", "", map[string]any{"email": "owner@acme.test", "password": "correct-horse-battery-staple"})
	if st != 200 {
		t.Fatalf("sign in: %d %s", st, b)
	}
	owner := field(b, "access_token").(string)

	// GoTrue serves its SAML service provider metadata, with the node's own key.
	st, b, _ = call("GET", gt+"/sso/saml/metadata", "", nil)
	if st != 200 || !strings.Contains(string(b), `entityID="http://api.supavise.test/auth/v1/sso/saml/metadata"`) || !strings.Contains(string(b), "X509Certificate") {
		t.Fatalf("SP metadata: %d %s", st, b)
	}

	// Nobody can sign up any other way: the hook refuses.
	st, b, _ = call("POST", gt+"/signup", "", map[string]any{"email": "intruder@acme.test", "password": "correct-horse-battery-staple"})
	if st < 400 || !strings.Contains(string(b), "Sign-up is closed") {
		t.Fatalf("a public sign-up: %d %s", st, b)
	}
	st, b, _ = call("POST", gt+"/otp", "", map[string]any{"email": "intruder2@acme.test", "create_user": true})
	if st < 400 {
		t.Fatalf("a sign-up by one-time code: %d %s", st, b)
	}
	if users, _ := srv.accounts.ListUsers(ctx); len(users) != 1 {
		t.Fatalf("the dashboard has %d users after refused sign-ups, want the one administrator", len(users))
	}

	// Register the identity provider (an Owner, through the API).
	_, md1, _ := call("GET", fmt.Sprintf("http://127.0.0.1:%d/metadata", idpPort), "", nil)
	st, b, _ = call("POST", adm+"/platform/organizations/acme/sso/providers", owner, map[string]any{
		"type": "saml", "metadata_xml": string(md1), "domains": []string{"acme.test"}, "default_role": "developer"})
	if st != 201 {
		t.Fatalf("add provider: %d %s", st, b)
	}
	provider := field(b, "id").(string)

	// The browser's walk: GoTrue's /sso, the identity provider's login, the assertion posted back.
	signIn := func(gt string, idpPort int, email string) (token string, redirect string) {
		t.Helper()
		st, b, _ := call("POST", gt+"/sso", "", map[string]any{"domain": email[strings.Index(email, "@")+1:], "skip_http_redirect": true,
			"redirect_to": "http://studio.supavise.test/sign-in-mfa?method=sso"})
		if st != 200 {
			t.Fatalf("sso: %d %s", st, b)
		}
		idpURL := field(b, "url").(string)
		if !strings.HasPrefix(idpURL, fmt.Sprintf("http://127.0.0.1:%d/sso?", idpPort)) {
			t.Fatalf("GoTrue sends the browser to %s", idpURL)
		}
		st, b, _ = call("GET", idpURL, "", nil)
		if st != 200 {
			t.Fatalf("idp: %d %s", st, b)
		}
		form := url.Values{"email": {email}}
		for _, m := range inputRe.FindAllStringSubmatch(string(b), -1) {
			form.Set(m[1], html.UnescapeString(m[2]))
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("http://127.0.0.1:%d/login", idpPort), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("login: %d %s", resp.StatusCode, page)
		}
		acs := html.UnescapeString(formActionRe.FindStringSubmatch(string(page))[1])
		post := url.Values{}
		for _, m := range inputRe.FindAllStringSubmatch(string(page), -1) {
			post.Set(m[1], html.UnescapeString(m[2]))
		}
		// The assertion goes to the ACS URL the browser was given, behind the proxy's prefix.
		u, _ := url.Parse(acs)
		if u.Path != "/auth/v1/sso/saml/acs" {
			t.Fatalf("the ACS URL is %s", acs)
		}
		req, _ = http.NewRequestWithContext(ctx, "POST", gt+"/sso/saml/acs", strings.NewReader(post.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err = hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		lu, err := url.Parse(loc)
		if err != nil || resp.StatusCode/100 != 3 {
			t.Fatalf("ACS answered %d %q", resp.StatusCode, loc)
		}
		frag, _ := url.ParseQuery(lu.Fragment)
		return frag.Get("access_token"), loc
	}

	// An allowed domain: the default role on the first sign-in.
	alice, loc := signIn(gt, idpPort, "alice@acme.test")
	if alice == "" {
		t.Fatalf("no session came back: %s", loc)
	}
	claims := jwtClaims(t, alice)
	if app, _ := claims["app_metadata"].(map[string]any); app["provider"] != "sso:"+provider {
		t.Fatalf("app_metadata = %v", claims["app_metadata"])
	}
	if st, b, _ := call("GET", adm+"/platform/profile", alice, nil); st != 200 {
		t.Fatalf("an allowed domain's first request: %d %s", st, b)
	}
	st, b, _ = call("GET", adm+"/platform/organizations/acme/members", owner, nil)
	var roleOfAlice any
	var list []any
	_ = json.Unmarshal(b, &list)
	for _, m := range list {
		mm := m.(map[string]any)
		if mm["primary_email"] == "alice@acme.test" {
			roleOfAlice = mm["role_ids"]
		}
	}
	if st != 200 || fmt.Sprint(roleOfAlice) != "[3]" {
		t.Fatalf("members: %d role of alice %v: %s", st, roleOfAlice, b)
	}
	if st, _, _ := call("PATCH", adm+"/v1/projects/abcdefghijklmnopqrst/config/auth", alice, map[string]any{"site_url": "https://x.test"}); st != 403 && st != 404 {
		t.Fatalf("a Developer saving settings: %d", st)
	}

	// Any other domain: GoTrue finds no provider for it.
	if st, b, _ := call("POST", gt+"/sso", "", map[string]any{"domain": "contractor.test", "skip_http_redirect": true}); st != 404 {
		t.Fatalf("a domain without a provider: %d %s", st, b)
	}
	// An address of another domain signed in through the registered provider: the identity
	// provider vouches for it, the dashboard does not know the domain.
	_, md2, _ := call("GET", fmt.Sprintf("http://127.0.0.1:%d/metadata", idp2Port), "", nil)
	if st, b, _ := call("POST", adm+"/platform/organizations/acme/sso/providers", owner, map[string]any{
		"type": "saml", "metadata_xml": string(md2), "domains": []string{"contractor.test"}}); st != 201 { // no default role
		t.Fatalf("second provider: %d %s", st, b)
	}
	bob, _ := signIn(gt, idp2Port, "bob@contractor.test")
	if bob == "" {
		t.Fatal("no session for the second provider's user")
	}
	for _, path := range []string{"/platform/profile", "/platform/projects", "/v1/projects"} {
		if st, _, _ := call("GET", adm+path, bob, nil); st != 403 {
			t.Fatalf("a user waiting for approval, GET %s: %d", path, st)
		}
	}
	st, b, _ = call("GET", adm+"/platform/organizations/acme/sso/pending", owner, nil)
	if st != 200 || !strings.Contains(string(b), "bob@contractor.test") {
		t.Fatalf("pending: %d %s", st, b)
	}
	userID := field([]byte(strings.TrimSuffix(strings.TrimPrefix(string(b), `{"items":[`), `]}`)), "user_id").(string)
	if st, b, _ := call("POST", adm+"/platform/organizations/acme/sso/pending/"+userID, owner, map[string]any{"role": "read-only"}); st != 200 {
		t.Fatalf("approve: %d %s", st, b)
	}
	if st, b, _ := call("GET", adm+"/platform/profile", bob, nil); st != 200 {
		t.Fatalf("after the approval: %d %s", st, b)
	}

	// Removing the provider ends the session of its users.
	if st, b, _ := call("DELETE", adm+"/platform/organizations/acme/sso/providers/"+provider, owner, nil); st != 200 {
		t.Fatalf("remove: %d %s", st, b)
	}
	time.Sleep(ssoCacheTTL + time.Second)
	if st, _, _ := call("GET", adm+"/platform/profile", alice, nil); st != 403 {
		t.Fatalf("alice after the provider was removed: %d", st)
	}

	// A project's own identity providers: SAML is a setting of the project's Auth config (off
	// until enabled, as on hosted), the project's GoTrue starts with its own signing key, and
	// the provider is managed through the Management API like on hosted.
	proj, err := node.Engine.Create(ctx, lifecycle.CreateRequest{Name: "shop", Class: "micro", OrgSlug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	pgt := fmt.Sprintf("http://127.0.0.1:%d", cfg.PortsFor(proj.Ref, proj.Seq).GoTrue)
	pbase := adm + "/v1/projects/" + proj.Ref + "/config/auth/sso/providers"
	if st, b, _ := call("GET", pbase, owner, nil); st != 404 || !strings.Contains(string(b), "SAML 2.0 support is not enabled") {
		t.Fatalf("project SSO before SAML is enabled: %d %s", st, b)
	}
	if st, _, _ := call("GET", pgt+"/sso/saml/metadata", "", nil); st != 404 {
		t.Fatalf("a project's SAML metadata while SAML is off: %d", st)
	}
	if st, b, _ := call("PATCH", adm+"/v1/projects/"+proj.Ref+"/config/auth", owner, map[string]any{"saml_enabled": true}); st != 200 {
		t.Fatalf("enabling SAML: %d %s", st, b)
	}
	st, pmeta, _ := call("GET", pgt+"/sso/saml/metadata", "", nil)
	_, smeta, _ := call("GET", gt+"/sso/saml/metadata", "", nil)
	if st != 200 || !strings.Contains(string(pmeta), "X509Certificate") {
		t.Fatalf("the project's SAML metadata: %d %s", st, pmeta)
	}
	cert := func(b []byte) string {
		m := regexp.MustCompile(`<(?:\w+:)?X509Certificate[^>]*>([^<]+)<`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("no certificate in %s", b)
		}
		return string(m[1])
	}
	if cert(pmeta) == cert(smeta) {
		t.Fatal("the project signs with the dashboard's key")
	}
	if !strings.Contains(string(pmeta), "http://"+proj.Ref+".api.supavise.test/auth/v1/sso/saml/metadata") {
		t.Fatalf("the project's entity id: %s", pmeta)
	}
	st, b, _ = call("POST", pbase, owner, map[string]any{"type": "saml", "metadata_xml": string(md1), "domains": []string{"shop.test"},
		"attribute_mapping": map[string]any{"keys": map[string]any{"email": map[string]any{"name": "email"}}}})
	if st != 201 {
		t.Fatalf("create project provider: %d %s", st, b)
	}
	pprov := field(b, "id").(string)
	if st, b, _ := call("GET", pbase, owner, nil); st != 200 || !strings.Contains(string(b), pprov) || !strings.Contains(string(b), "metadata_xml") {
		t.Fatalf("list project providers: %d %s", st, b)
	}
	carol, loc := signIn(pgt, idpPort, "carol@shop.test")
	if carol == "" {
		t.Fatalf("no session for the project's end user: %s", loc)
	}
	cc := jwtClaims(t, carol)
	if app, _ := cc["app_metadata"].(map[string]any); app["provider"] != "sso:"+pprov || cc["email"] != "carol@shop.test" {
		t.Fatalf("project end user claims: %v", cc)
	}
	// The project's end users are not dashboard users, and signed in nowhere else.
	if st, _, _ := call("GET", adm+"/platform/profile", carol, nil); st != 401 {
		t.Fatalf("a project's end user on the dashboard API: %d", st)
	}
	if st, b, _ := call("DELETE", pbase+"/"+pprov, owner, nil); st != 200 {
		t.Fatalf("delete project provider: %d %s", st, b)
	}
	if st, _, _ := call("GET", pbase+"/"+pprov, owner, nil); st != 404 {
		t.Fatalf("project provider after delete: %d", st)
	}

	// The hook fails closed: while the daemon does not answer, GoTrue creates nobody.
	admin.Close()
	time.Sleep(500 * time.Millisecond)
	before, _ := srv.accounts.ListUsers(ctx)
	st, b, _ = call("POST", gt+"/signup", "", map[string]any{"email": "late@acme.test", "password": "correct-horse-battery-staple"})
	if st < 400 {
		t.Fatalf("a sign-up while the daemon is down: %d %s", st, b)
	}
	if after, _ := srv.accounts.ListUsers(ctx); len(after) != len(before) {
		t.Fatalf("a user was created while the hook could not answer")
	}
}

// distinctFreePorts returns n different free loopback ports from the range freePort uses.
func distinctFreePorts(t *testing.T, n int) []int {
	t.Helper()
	var ls []net.Listener
	var out []int
	for p := portBase() + int(time.Now().UnixNano()%300); len(out) < n && p < portBase()+900; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		ls = append(ls, l)
		out = append(out, p)
	}
	for _, l := range ls {
		l.Close()
	}
	if len(out) < n {
		t.Fatal("no free ports")
	}
	return out
}

// jwtClaims decodes the claims of a JWT without checking it.
func jwtClaims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}
