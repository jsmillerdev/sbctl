package awsfake

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
)

// Scope is who signed a request and for what.
type Scope struct {
	AccessKeyID string
	Date        string
	Region      string
	Service     string
}

// Verify checks the SigV4 signature on a request received by a server, the way an AWS endpoint
// does, and returns the scope it was signed for. lookup returns the credentials for an access key
// id. body is the request payload.
//
// It signs a copy of the request that carries only the headers the request names as signed, with
// awsapi.Sign, and compares the result. So it accepts what the client signs: Host, X-Amz-Date and
// every other header except the ones Sign leaves out (Authorization, User-Agent, X-Amzn-Trace-Id,
// Expect, Connection, Transfer-Encoding and Content-Length). A request that signs one of those is
// refused.
func Verify(r *http.Request, body []byte, lookup func(accessKeyID string) (awsapi.Credentials, bool)) (Scope, error) {
	auth := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		return Scope{}, errors.New("no AWS4-HMAC-SHA256 Authorization header")
	}
	fields := authFields(rest)
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[4] != "aws4_request" {
		return Scope{}, fmt.Errorf("malformed credential scope %q", fields["Credential"])
	}
	sc := Scope{AccessKeyID: cred[0], Date: cred[1], Region: cred[2], Service: cred[3]}
	creds, found := lookup(sc.AccessKeyID)
	if !found {
		return sc, fmt.Errorf("unknown access key id %q", sc.AccessKeyID)
	}
	if creds.SessionToken != r.Header.Get("X-Amz-Security-Token") {
		return sc, errors.New("the session token does not match the access key")
	}
	stamp := r.Header.Get("X-Amz-Date")
	t, err := time.Parse("20060102T150405Z", stamp)
	if err != nil || t.Format("20060102") != sc.Date {
		return sc, fmt.Errorf("X-Amz-Date %q does not match the credential scope date %q", stamp, sc.Date)
	}
	names := strings.Split(fields["SignedHeaders"], ";")
	signed := map[string]bool{}
	for _, n := range names {
		signed[n] = true
	}
	for _, need := range []string{"host", "x-amz-date"} {
		if !signed[need] {
			return sc, fmt.Errorf("the %s header is not signed", need)
		}
	}
	if r.Header.Get("X-Amz-Security-Token") != "" && !signed["x-amz-security-token"] {
		return sc, errors.New("the x-amz-security-token header is not signed")
	}

	signedOnly := http.Header{}
	for _, n := range names {
		if n == "host" {
			continue
		}
		vals := r.Header.Values(n)
		if len(vals) == 0 {
			return sc, fmt.Errorf("the signed header %s is not on the request", n)
		}
		signedOnly[http.CanonicalHeaderKey(n)] = vals
	}
	again := &http.Request{Method: r.Method, URL: r.URL, Host: r.Host, Header: signedOnly}
	awsapi.Sign(again, body, creds, sc.Region, sc.Service, t)
	want := authFields(strings.TrimPrefix(again.Header.Get("Authorization"), "AWS4-HMAC-SHA256 "))
	if want["SignedHeaders"] != fields["SignedHeaders"] {
		return sc, fmt.Errorf("the signed headers %q are not the ones awsapi.Sign signs for this request (%q)", fields["SignedHeaders"], want["SignedHeaders"])
	}
	if !hmac.Equal([]byte(want["Signature"]), []byte(fields["Signature"])) {
		return sc, errors.New("the request signature does not match")
	}
	return sc, nil
}

// authFields splits "Credential=..., SignedHeaders=..., Signature=..." into its fields.
func authFields(s string) map[string]string {
	fields := map[string]string{}
	for _, f := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		fields[k] = v
	}
	return fields
}
