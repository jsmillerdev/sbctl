package awsfake_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

var (
	verifyAt    = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	verifyBody  = []byte("Action=DescribeInstances&Version=2016-11-15")
	verifyCreds = awsapi.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: "session-token"}
)

func lookupVerifyCreds(id string) (awsapi.Credentials, bool) {
	return verifyCreds, id == verifyCreds.AccessKeyID
}

// signedRequest is a request as the client signs it, with the headers the transport adds on top.
func signedRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://ec2.us-east-1.amazonaws.com/?x=1", strings.NewReader(string(verifyBody)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	awsapi.Sign(req, verifyBody, verifyCreds, "us-east-1", "ec2", verifyAt)
	req.Header.Set("User-Agent", "supavise-awsapi")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Length", "43")
	return req
}

func TestVerifyAcceptsWhatSignProduces(t *testing.T) {
	sc, err := awsfake.Verify(signedRequest(t), verifyBody, lookupVerifyCreds)
	if err != nil || sc != (awsfake.Scope{AccessKeyID: "AKIDEXAMPLE", Date: "20261008", Region: "us-east-1", Service: "ec2"}) {
		t.Errorf("%+v, %v", sc, err)
	}
}

func TestVerifyRefusesWhatWasChanged(t *testing.T) {
	// auth edits the Authorization header into a different, still well-formed one.
	auth := func(from, to string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), from, to, 1))
		}
	}
	for name, tc := range map[string]struct {
		change func(*http.Request)
		body   []byte
		creds  func(string) (awsapi.Credentials, bool)
		want   string
	}{
		"body":          {func(*http.Request) {}, []byte("Action=StopInstances"), lookupVerifyCreds, "signature does not match"},
		"query":         {func(r *http.Request) { r.URL.RawQuery = "x=2" }, verifyBody, lookupVerifyCreds, "signature does not match"},
		"method":        {func(r *http.Request) { r.Method = http.MethodGet }, verifyBody, lookupVerifyCreds, "signature does not match"},
		"signed header": {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, verifyBody, lookupVerifyCreds, "signature does not match"},
		"host":          {func(r *http.Request) { r.Host = "ec2.eu-west-1.amazonaws.com" }, verifyBody, lookupVerifyCreds, "signature does not match"},
		"secret": {func(*http.Request) {}, verifyBody, func(id string) (awsapi.Credentials, bool) {
			c := verifyCreds
			c.SecretAccessKey = "another secret"
			return c, true
		}, "signature does not match"},
		"unknown key":           {func(*http.Request) {}, verifyBody, func(string) (awsapi.Credentials, bool) { return awsapi.Credentials{}, false }, `unknown access key id "AKIDEXAMPLE"`},
		"session token":         {func(r *http.Request) { r.Header.Set("X-Amz-Security-Token", "another") }, verifyBody, lookupVerifyCreds, "session token does not match"},
		"date":                  {func(r *http.Request) { r.Header.Set("X-Amz-Date", "20261009T120000Z") }, verifyBody, lookupVerifyCreds, "does not match the credential scope date"},
		"region":                {auth("/us-east-1/", "/eu-west-1/"), verifyBody, lookupVerifyCreds, "signature does not match"},
		"service":               {auth("/ec2/", "/sts/"), verifyBody, lookupVerifyCreds, "signature does not match"},
		"scope":                 {auth("/aws4_request", "/aws5_request"), verifyBody, lookupVerifyCreds, "malformed credential scope"},
		"no signature":          {func(r *http.Request) { r.Header.Del("Authorization") }, verifyBody, lookupVerifyCreds, "no AWS4-HMAC-SHA256 Authorization header"},
		"host unsigned":         {auth("content-type;host;", "content-type;"), verifyBody, lookupVerifyCreds, "host header is not signed"},
		"date unsigned":         {auth(";x-amz-date", ""), verifyBody, lookupVerifyCreds, "x-amz-date header is not signed"},
		"token unsigned":        {auth(";x-amz-security-token", ""), verifyBody, lookupVerifyCreds, "x-amz-security-token header is not signed"},
		"signed header removed": {func(r *http.Request) { r.Header.Del("Content-Type") }, verifyBody, lookupVerifyCreds, "signed header content-type is not on the request"},
		"signature": {func(r *http.Request) {
			a := r.Header.Get("Authorization")
			r.Header.Set("Authorization", a[:len(a)-1]+"0")
			if strings.HasSuffix(a, "0") {
				r.Header.Set("Authorization", a[:len(a)-1]+"1")
			}
		}, verifyBody, lookupVerifyCreds, "signature does not match"},
		"a header Sign never signs": {auth("content-type;host", "content-length;content-type;host"), verifyBody, lookupVerifyCreds, "are not the ones awsapi.Sign signs"},
	} {
		t.Run(name, func(t *testing.T) {
			r := signedRequest(t)
			tc.change(r)
			if _, err := awsfake.Verify(r, tc.body, tc.creds); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
