// Package awsfake is an in-process fake of the AWS endpoints that package awsapi calls: EC2,
// Secrets Manager, STS and the instance metadata service, all on one loopback listener. It checks
// every signature with Verify, so a test notices a client that signs the wrong thing or
// uses the wrong credentials, and it records every call in arrival order, so a test can assert
// that a fencer stopped the peer, waited for "stopped" and only then took the address.
//
// A test starts one with New, seeds it (AddInstance, AddAddress, AddSecret, AddRole, SetIMDS),
// points the code under test at it (SetEnv for code that builds its own awsapi client, or
// Client and Config for code that is handed one), and reads Calls or Order afterwards. The state
// model is the part of AWS that the failover design depends on: StopInstances moves an instance to
// stopping and then, after a chosen number of polls, to stopped (or never, without Force);
// AssociateAddress moves an Elastic IP and releases the one it replaces; DryRun checks permission
// and changes nothing.
package awsfake

import (
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
)

// Call is one request the fake received.
type Call struct {
	Seq     int
	Service string // "ec2", "secretsmanager", "sts" or "imds"
	// Action is the API action, or "GET /latest/meta-data/instance-id" for the metadata service.
	Action string
	// Params are the request's fields: the decoded form for EC2 and STS, the top-level string
	// fields of the JSON body for Secrets Manager.
	Params      url.Values
	DryRun      bool
	AccessKeyID string
	Status      int    // the HTTP status the fake answered with
	Code        string // the error code it answered with, if any
}

// Fault makes calls fail with an API error.
type Fault struct {
	Status  int
	Code    string
	Message string
	// Times is how many calls fail; zero means every call.
	Times int
	// Applied makes an EC2 call take effect before it fails, like an answer lost on the way: the
	// client sends the request again and finds the work done. A DryRun call has no effect to apply.
	Applied bool
}

type fault struct {
	Fault
	service, action string
	used            int
}

// Server is the fake. Its methods are safe to call from several goroutines.
type Server struct {
	t   testing.TB
	srv *http.Server
	url string

	mu         sync.Mutex
	seq        int
	calls      []Call
	creds      map[string]awsapi.Credentials
	instances  map[string]*instance
	addresses  []*Address
	secrets    []Secret
	roles      map[string]bool
	imds       IMDSData
	imdsTokens map[string]bool
	faults     []*fault
	pageSize   int
	counter    int
}

// Static credentials the fake always accepts, for tests that hand a client explicit ones.
func staticCredentials() awsapi.Credentials {
	return awsapi.Credentials{AccessKeyID: "AKIAFAKESTATIC00000", SecretAccessKey: "fake-static-secret"}
}

func roleCredentials() awsapi.Credentials {
	return awsapi.Credentials{AccessKeyID: "ASIAFAKEROLE0000000", SecretAccessKey: "fake-role-secret", SessionToken: "fake-role-token"}
}

// New starts a fake on 127.0.0.1 and stops it when the test ends. The metadata service describes
// an instance "i-0aaaaaaaaaaaaaaaa" in us-east-1a with the role "supavise-instance-role".
func New(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("awsfake: %v", err)
	}
	s := &Server{
		t:          t,
		url:        "http://" + ln.Addr().String(),
		creds:      map[string]awsapi.Credentials{},
		instances:  map[string]*instance{},
		roles:      map[string]bool{},
		imdsTokens: map[string]bool{},
		imds: IMDSData{
			InstanceID: "i-0aaaaaaaaaaaaaaaa", Region: "us-east-1", AZ: "us-east-1a", LocalIP: "10.77.0.10",
			Role: "supavise-instance-role", CredentialsTTL: 6 * time.Hour,
		},
	}
	for _, c := range []awsapi.Credentials{staticCredentials(), roleCredentials()} {
		s.creds[c.AccessKeyID] = c
	}
	s.srv = &http.Server{Handler: s}
	go s.srv.Serve(ln)
	t.Cleanup(func() { s.srv.Close() })
	return s
}

// URL is the base URL for every service.
func (s *Server) URL() string { return s.url }

// Region is the region the fake expects in credential scopes.
func (s *Server) Region() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.imds.Region
}

// Credentials are static credentials the fake accepts.
func (s *Server) Credentials() awsapi.Credentials { return staticCredentials() }

// AddCredentials makes the fake accept another set of credentials, with its session token.
func (s *Server) AddCredentials(c awsapi.Credentials) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[c.AccessKeyID] = c
}

// Env is the environment that points awsapi at the fake. It carries no credentials: the client
// then takes them from the instance role through the fake metadata service, as it does on EC2.
func (s *Server) Env() map[string]string {
	return map[string]string{
		awsapi.EnvEndpointEC2:            s.url,
		awsapi.EnvEndpointSecretsManager: s.url,
		awsapi.EnvEndpointSTS:            s.url,
		awsapi.EnvEndpointIMDS:           s.url,
		"AWS_REGION":                     s.Region(),
	}
}

// SetEnv sets Env for the test and clears the variables that would give the client credentials,
// turn the metadata service off or keep the client from using the instance role.
func (s *Server) SetEnv(t testing.TB) {
	t.Helper()
	for k, v := range s.Env() {
		t.Setenv(k, v)
	}
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_DEFAULT_REGION", "AWS_EC2_METADATA_DISABLED", awsapi.EnvNoInstanceRole} {
		t.Setenv(k, "")
	}
}

// Config is a client configuration for the fake that ignores the real environment. With no
// Credentials set the client uses the instance role.
func (s *Server) Config() awsapi.Config {
	return awsapi.Config{
		Region:       s.Region(),
		Endpoints:    awsapi.Endpoints{EC2: s.url, SecretsManager: s.url, STS: s.url, IMDS: s.url},
		Getenv:       func(string) string { return "" },
		RetryBackoff: time.Millisecond,
	}
}

// Client is a client for the fake, built from Config.
func (s *Server) Client() *awsapi.Client {
	s.t.Helper()
	c, err := awsapi.New(s.Config())
	if err != nil {
		s.t.Fatalf("awsfake: %v", err)
	}
	return c
}

// Calls returns every call received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Order lists the calls as "ec2:StopInstances" in arrival order; a DryRun call is
// "ec2:StopInstances(dryrun)". With no arguments it leaves out the metadata service; name the
// services to keep only those ("imds" included).
func (s *Server) Order(services ...string) []string {
	keep := map[string]bool{}
	for _, svc := range services {
		keep[svc] = true
	}
	var out []string
	for _, c := range s.Calls() {
		if len(keep) == 0 && c.Service == "imds" || len(keep) > 0 && !keep[c.Service] {
			continue
		}
		name := c.Service + ":" + c.Action
		if c.DryRun {
			name += "(dryrun)"
		}
		out = append(out, name)
	}
	return out
}

// Inject makes calls to a service action fail. For the metadata service the action is the URL path.
func (s *Server) Inject(service, action string, f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{Fault: f, service: service, action: action})
}

// Deny makes the actions fail the way a role without the permission does, for DryRun and real
// calls alike.
func (s *Server) Deny(service string, actions ...string) {
	f := Fault{Status: http.StatusForbidden, Code: "UnauthorizedOperation", Message: "You are not authorized to perform this operation."}
	switch service {
	case "secretsmanager":
		f.Status, f.Code = http.StatusBadRequest, "AccessDeniedException"
	case "sts":
		f.Code = "AccessDenied"
	}
	for _, a := range actions {
		s.Inject(service, a, f)
	}
}

// SetPageSize makes DescribeInstances and DescribeInstanceStatus return at most n results per
// page, with a NextToken. Zero turns paging off.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// ExpireIMDSTokens makes the metadata service answer 401 to every session token issued so far.
func (s *Server) ExpireIMDSTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imdsTokens = map[string]bool{}
}

// AddRole allows AssumeRole for the role ARN.
func (s *Server) AddRole(arn string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[arn] = true
}

// next returns a counter for ids; the caller holds mu.
func (s *Server) next() int {
	s.counter++
	return s.counter
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/latest/") {
		s.serveIMDS(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonAPI := strings.HasPrefix(r.Header.Get("X-Amz-Target"), "secretsmanager.")
	s.mu.Lock()
	sc, verr := Verify(r, body, func(id string) (awsapi.Credentials, bool) { c, ok := s.creds[id]; return c, ok })
	s.mu.Unlock()
	rec := Call{AccessKeyID: sc.AccessKeyID, Service: sc.Service}
	if jsonAPI {
		rec.Service = "secretsmanager"
		rec.Action = strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "secretsmanager.")
		rec.Params = jsonParams(body)
	} else if q, err := url.ParseQuery(string(body)); err == nil {
		rec.Params, rec.Action = q, q.Get("Action")
		rec.DryRun = q.Get("DryRun") == "true"
	}
	var res result
	switch {
	case verr != nil:
		res = fail(http.StatusForbidden, "SignatureDoesNotMatch", verr.Error())
	case rec.Service == "ec2":
		res = s.ec2(rec)
	case rec.Service == "sts":
		res = s.sts(rec)
	case rec.Service == "secretsmanager":
		res = s.secretsManager(rec)
	default:
		res = fail(http.StatusBadRequest, "UnknownService", fmt.Sprintf("service %q", rec.Service))
	}
	s.record(&rec, res)
	s.write(w, rec.Service, res)
}

func (s *Server) record(c *Call, res result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	c.Seq, c.Status, c.Code = s.seq, res.status, res.code
	s.calls = append(s.calls, *c)
}

// result is the answer to one call: an XML or JSON body, or an error.
type result struct {
	status  int
	body    string
	code    string
	message string
}

func success(body string) result { return result{status: http.StatusOK, body: body} }

func fail(status int, code, message string) result {
	return result{status: status, code: code, message: message}
}

// write sends res in the protocol of the service: EC2 and STS errors are XML, Secrets Manager's JSON.
func (s *Server) write(w http.ResponseWriter, service string, res result) {
	switch {
	case res.code == "":
		w.Header().Set("Content-Type", contentType(service))
		w.WriteHeader(res.status)
		io.WriteString(w, res.body)
	case service == "secretsmanager":
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.Header().Set("X-Amzn-ErrorType", res.code)
		w.WriteHeader(res.status)
		fmt.Fprintf(w, `{"__type":%q,"Message":%q}`, res.code, res.message)
	case service == "sts":
		w.WriteHeader(res.status)
		fmt.Fprintf(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>fake-request</RequestId></ErrorResponse>`, esc(res.code), esc(res.message))
	default:
		w.WriteHeader(res.status)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors><RequestID>fake-request</RequestID></Response>`, esc(res.code), esc(res.message))
	}
}

func contentType(service string) string {
	if service == "secretsmanager" {
		return "application/x-amz-json-1.1"
	}
	return "text/xml;charset=UTF-8"
}

// injected returns the fault that applies to the call, if any, and counts the use.
func (s *Server) injected(service, action string) (result, bool) {
	f, hit := s.nextFault(service, action)
	return fail(f.Status, f.Code, f.Message), hit
}

func (s *Server) nextFault(service, action string) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.service != service || f.action != action || (f.Times > 0 && f.used >= f.Times) {
			continue
		}
		f.used++
		return f.Fault, true
	}
	return Fault{}, false
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
