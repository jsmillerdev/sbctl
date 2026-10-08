package awsapi

import (
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The vectors are the cases of AWS's published Signature Version 4 test suite
// (aws-sig-v4-test-suite, as kept in botocore's tests/unit/auth/aws4_testsuite, Apache-2.0):
// the request, the canonical request and the Authorization header for the credentials
// AKIDEXAMPLE / wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY, region us-east-1, service "service".
// Cases that repeat another case's data are left out.
const (
	suiteAccessKey = "AKIDEXAMPLE"
	suiteSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	suiteAuthz     = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, "
	suiteEmptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	suiteToken     = "6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267"
)

type suiteCase struct {
	name  string
	req   string
	creq  string
	authz string
}

var suiteCases = []suiteCase{
	{"get-vanilla",
		"GET / HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{"get-vanilla-empty-query-key",
		"GET /?Param1=value1 HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\nParam1=value1\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=a67d582fa61cc504c4bae71f336f98b97f1ea3c7a6bfe1b6e45aec72011b9aeb"},
	{"get-vanilla-query-order-key-case",
		"GET /?Param2=value2&Param1=value1 HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\nParam1=value1&Param2=value2\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
	{"get-vanilla-query-order-key",
		"GET /?Param1=value2&Param1=Value1 HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\nParam1=Value1&Param1=value2\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=eedbc4e291e521cf13422ffca22be7d2eb8146eecf653089df300a15b2382bd1"},
	{"get-vanilla-query-order-value",
		"GET /?Param1=value2&Param1=value1 HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\nParam1=value1&Param1=value2\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5772eed61e12b33fae39ee5e7012498b51d56abc0abb7c60486157bd471c4694"},
	{"get-vanilla-query-order-encoded",
		"GET /?Param-3=Value3&Param=Value2&%E1%88%B4=Value1 HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z\n",
		"GET\n/\n%E1%88%B4=Value1&Param=Value2&Param-3=Value3\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=371d3713e185cc334048618a97f809c9ffe339c62934c032af5a0e595648fcac"},
	{"get-vanilla-query-unreserved",
		"GET /?-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz=-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz=-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=9c3e54bfcdf0b19771a7f523ee5669cdf59bc7cc0884027167c21bb143a40197"},
	{"get-vanilla-utf8-query",
		"GET /?ሴ=bar HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n%E1%88%B4=bar\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04"},
	{"get-vanilla-with-session-token",
		"GET / HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\nx-amz-security-token:" + suiteToken + "\n\nhost;x-amz-date;x-amz-security-token\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8"},
	{"get-unreserved",
		"GET /-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=07ef7494c76fa4850883e2b006601f940f8a34d404d0cfa977f52a65bbf5f24f"},
	{"get-utf8",
		"GET /ሴ HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/%E1%88%B4\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=8318018e0b0f223aa2bbf98705b62bb787dc9c0e678f255a891fd03141be5d85"},
	{"get-header-key-duplicate",
		"GET / HTTP/1.1\nHost:example.amazonaws.com\nMy-Header1:value2\nMy-Header1:value2\nMy-Header1:value1\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nmy-header1:value2,value2,value1\nx-amz-date:20150830T123600Z\n\nhost;my-header1;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;my-header1;x-amz-date, Signature=c9d5ea9f3f72853aea855b47ea873832890dbdd183b4468f858259531a5138ea"},
	{"get-header-value-multiline",
		"GET / HTTP/1.1\nHost:example.amazonaws.com\nMy-Header1:value1\n  value2\n     value3\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nmy-header1:value1 value2 value3\nx-amz-date:20150830T123600Z\n\nhost;my-header1;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;my-header1;x-amz-date, Signature=cfd34249e4b1c8d6b91ef74165d41a32e5fab3306300901bb65a51a73575eefd"},
	{"get-header-value-order",
		"GET / HTTP/1.1\nHost:example.amazonaws.com\nMy-Header1:value4\nMy-Header1:value1\nMy-Header1:value3\nMy-Header1:value2\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nmy-header1:value4,value1,value3,value2\nx-amz-date:20150830T123600Z\n\nhost;my-header1;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;my-header1;x-amz-date, Signature=08c7e5a9acfcfeb3ab6b2185e75ce8b1deb5e634ec47601a50643f830c755c01"},
	{"get-header-value-trim",
		"GET / HTTP/1.1\nHost:example.amazonaws.com\nMy-Header1: value1\nMy-Header2: \"a   b   c\"\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nmy-header1:value1\nmy-header2:\"a b c\"\nx-amz-date:20150830T123600Z\n\nhost;my-header1;my-header2;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;my-header1;my-header2;x-amz-date, Signature=acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736"},
	{"post-vanilla",
		"POST / HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"POST\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"},
	{"post-vanilla-query",
		"POST /?Param1=value1 HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"POST\n/\nParam1=value1\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11"},
	{"post-header-key-sort",
		"POST / HTTP/1.1\nHost:example.amazonaws.com\nMy-Header1:value1\nX-Amz-Date:20150830T123600Z",
		"POST\n/\n\nhost:example.amazonaws.com\nmy-header1:value1\nx-amz-date:20150830T123600Z\n\nhost;my-header1;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;my-header1;x-amz-date, Signature=c5410059b04c1ee005303aed430f6e6645f61f4dc9e1461ec8f8916fdf18852c"},
	{"post-header-value-case",
		"POST / HTTP/1.1\nHost:example.amazonaws.com\nMy-Header1:VALUE1\nX-Amz-Date:20150830T123600Z",
		"POST\n/\n\nhost:example.amazonaws.com\nmy-header1:VALUE1\nx-amz-date:20150830T123600Z\n\nhost;my-header1;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;my-header1;x-amz-date, Signature=cdbc9802e29d2942e5e10b5bccfdd67c5f22c7c4e8ae67b53629efa58b974b7d"},
	{"post-x-www-form-urlencoded",
		"POST / HTTP/1.1\nContent-Type:application/x-www-form-urlencoded\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z\n\nParam1=value1",
		"POST\n/\n\ncontent-type:application/x-www-form-urlencoded\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
		suiteAuthz + "SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"},
	{"post-x-www-form-urlencoded-parameters",
		"POST / HTTP/1.1\nContent-Type:application/x-www-form-urlencoded; charset=utf8\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z\n\nParam1=value1",
		"POST\n/\n\ncontent-type:application/x-www-form-urlencoded; charset=utf8\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
		suiteAuthz + "SignedHeaders=content-type;host;x-amz-date, Signature=1a72ec8f64bd914b0e42e42607c7fbce7fb2c7465f63e3092b3b0d39fa77a6fe"},
	{"post-sts-header-before",
		"POST / HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z\nX-Amz-Security-Token:AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA==",
		"POST\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\nx-amz-security-token:AQoDYXdzEPT//////////wEXAMPLEtc764bNrC9SAPBSM22wDOk4x4HIZ8j4FZTwdQWLWsKWHGBuFqwAeMicRXmxfpSPfIeoIYRqTflfKD8YUuwthAx7mSEI/qkPpKPi/kMcGdQrmGdeehM4IC1NtBmUpp2wUE8phUZampKsburEDy0KPkyQDYwT7WZ0wq5VSXDvp75YU9HFvlRd8Tx6q6fE8YQcHNVXAkiY9q6d+xo0rKwT38xVqr7ZD0u0iPPkUL64lIZbqBAz+scqKmlzm8FDrypNC9Yjc8fPOLn9FX9KSYvKTr4rvx3iSIlTJabIQwj2ICCR/oLxBA==\n\nhost;x-amz-date;x-amz-security-token\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=85d96828115b5dc0cfc3bd16ad9e210dd772bbebba041836c64533a82be05ead"},
	// normalize-path/
	{"get-relative",
		"GET /example/.. HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{"get-relative-relative",
		"GET /example1/example2/../.. HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{"get-slash",
		"GET // HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{"get-slash-dot-slash",
		"GET /./ HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{"get-slash-pointless-dot",
		"GET /./example HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/example\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=ef75d96142cf21edca26f06005da7988e4f8dc83a165a80865db7089db637ec5"},
	{"get-slashes",
		"GET //example// HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/example/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=9a624bd73a37c9a373b5312afbebe7a714a789de108f0bdfe846570885f57e84"},
	{"get-space",
		"GET /example space/ HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z",
		"GET\n/example%20space/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=652487583200325589f1fba4c7e578f72c47cb61beeca81406b39ddec1366741"},
	{"get-special-character",
		"GET /example/$delete HTTP/1.1\nHost:example.amazonaws.com\nX-Amz-Date:20150830T123600Z\n",
		"GET\n/example/%24delete\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + suiteEmptyHash,
		suiteAuthz + "SignedHeaders=host;x-amz-date, Signature=a853c9b21b528b19643d00910d35b83a10c366a10833ceefb45edd6c80e40f27"},
}

// parseSuiteRequest reads the suite's .req format: a request line, headers (a line that starts
// with white space continues the header above it), and after a blank line the body. The path is
// taken as written, so the suite's unencoded paths reach canonicalURI as the wire path.
func parseSuiteRequest(t *testing.T, raw string) (signRequest, time.Time) {
	t.Helper()
	head, body, _ := strings.Cut(raw, "\n\n")
	lines := strings.Split(strings.TrimRight(head, "\n"), "\n")
	method, rest, _ := strings.Cut(lines[0], " ")
	target := rest[:strings.LastIndexByte(rest, ' ')]
	path, query, _ := strings.Cut(target, "?")
	r := signRequest{Method: method, Path: path, Query: query, Header: http.Header{}, Body: []byte(body)}
	var name, value string
	flush := func() {
		if name == "" {
			return
		}
		if strings.EqualFold(name, "Host") {
			r.Host = value
		} else {
			r.Header.Add(name, value)
		}
	}
	for _, l := range lines[1:] {
		if l[0] == ' ' || l[0] == '\t' {
			value += " " + l
			continue
		}
		flush()
		name, value, _ = strings.Cut(l, ":")
	}
	flush()
	when, err := time.Parse(timeFormat, r.Header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatalf("X-Amz-Date: %v", err)
	}
	return r, when
}

func TestSigV4PublishedSuite(t *testing.T) {
	for _, c := range suiteCases {
		t.Run(c.name, func(t *testing.T) {
			r, when := parseSuiteRequest(t, c.req)
			creds := Credentials{AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecretKey}
			if c.name == "get-vanilla-with-session-token" {
				creds.SessionToken = suiteToken
			}
			stampHeaders(r.Header, creds, when)
			got := r.sign(creds, "us-east-1", "service", when, r.headerNames())
			if got.CanonicalRequest != c.creq {
				t.Errorf("canonical request\n got: %q\nwant: %q", got.CanonicalRequest, c.creq)
			}
			if got.Authorization != c.authz {
				t.Errorf("authorization\n got: %s\nwant: %s", got.Authorization, c.authz)
			}
		})
	}
}

// The examples in "Signing AWS API requests" use the same credentials with the IAM service.
func TestSigV4DocumentedExample(t *testing.T) {
	const date = "20150830"
	if got := hex.EncodeToString(signingKey(suiteSecretKey, date, "us-east-1", "iam")); got != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Errorf("signing key = %s", got)
	}
	u, _ := url.Parse("https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08")
	req := &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded; charset=utf-8"}}}
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	Sign(req, nil, Credentials{AccessKeyID: suiteAccessKey, SecretAccessKey: suiteSecretKey}, "us-east-1", "iam", when)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("authorization\n got: %s\nwant: %s", got, want)
	}
	if req.Header.Get("X-Amz-Date") != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q", req.Header.Get("X-Amz-Date"))
	}
}

func TestSignAddsTheSessionTokenAndSkipsTransportHeaders(t *testing.T) {
	u, _ := url.Parse("https://ec2.us-east-1.amazonaws.com/")
	req := &http.Request{Method: http.MethodPost, URL: u, Header: http.Header{
		"User-Agent": {"x"}, "Content-Length": {"3"}, "Accept-Encoding": {"gzip"},
	}}
	Sign(req, []byte("abc"), Credentials{AccessKeyID: "A", SecretAccessKey: "S", SessionToken: "T"}, "us-east-1", "ec2", time.Unix(0, 0))
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=accept-encoding;host;x-amz-date;x-amz-security-token,") {
		t.Errorf("signed headers: %s", auth)
	}
	if req.Header.Get("X-Amz-Security-Token") != "T" {
		t.Error("session token header missing")
	}
}

func TestCredentialsPrintWithoutTheSecret(t *testing.T) {
	c := Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "very-secret", SessionToken: "tok"}
	for _, s := range []string{c.String(), fmt.Sprintf("%v %+v %#v", c, c, c)} {
		if strings.Contains(s, "very-secret") || strings.Contains(s, "tok") || !strings.Contains(s, "AKIAEXAMPLE") {
			t.Errorf("printed credentials: %s", s)
		}
	}
}

// The production package only signs. Checking a signature is the fake's job (awsfake.Verify), so
// the daemon does not carry a verifier it never calls.
func TestProductionPackageOnlySigns(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "Verify" {
					t.Errorf("%s declares Verify", name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					if ts, ok := s.(*ast.TypeSpec); ok && ts.Name.Name == "Scope" {
						t.Errorf("%s declares Scope", name)
					}
				}
			}
		}
	}
}
