package awsapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	signAlgorithm = "AWS4-HMAC-SHA256"
	timeFormat    = "20060102T150405Z"
	dateFormat    = "20060102"
)

// ignoredHeaders are never signed: Authorization is the signature itself, and the rest are
// added or rewritten by the HTTP transport or a proxy after signing.
var ignoredHeaders = map[string]bool{
	"authorization":     true,
	"user-agent":        true,
	"x-amzn-trace-id":   true,
	"expect":            true,
	"connection":        true,
	"transfer-encoding": true,
	"content-length":    true,
}

// Sign signs req with Signature Version 4 for service in region. It sets X-Amz-Date, X-Amz-Security-Token
// (when creds has a session token) and Authorization, and signs every header the request carries
// except the ones in ignoredHeaders, plus Host. body is the exact payload the request will send.
//
// The canonical path is the request's escaped path encoded a second time, which is what every
// AWS service except S3 expects. The services in this package only use the path "/".
func Sign(req *http.Request, body []byte, creds Credentials, region, service string, now time.Time) {
	if req.Header == nil {
		req.Header = http.Header{}
	}
	t := now.UTC()
	stampHeaders(req.Header, creds, t)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	r := signRequest{Method: req.Method, Path: req.URL.EscapedPath(), Query: req.URL.RawQuery, Host: host, Header: req.Header, Body: body}
	s := r.sign(creds, region, service, t, r.headerNames())
	req.Header.Set("Authorization", s.Authorization)
}

// stampHeaders sets the headers a signature covers besides the ones the caller chose.
func stampHeaders(h http.Header, creds Credentials, t time.Time) {
	h.Set("X-Amz-Date", t.Format(timeFormat))
	if creds.SessionToken != "" {
		h.Set("X-Amz-Security-Token", creds.SessionToken)
	}
}

// signRequest is what a signature is computed over.
type signRequest struct {
	Method string
	Path   string // as it goes on the wire; SigV4 encodes it once more
	Query  string // raw query string without the "?"
	Host   string
	Header http.Header
	Body   []byte
}

// signed is a computed signature with the intermediate values that tests compare.
type signed struct {
	CanonicalRequest string
	StringToSign     string
	SignedHeaders    string
	Signature        string
	Authorization    string
}

// headerNames lists the lower-case names Sign covers: Host and every header that is not ignored.
func (r signRequest) headerNames() []string {
	seen := map[string]bool{"host": true}
	names := []string{"host"}
	for k := range r.Header {
		n := strings.ToLower(k)
		if ignoredHeaders[n] || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (r signRequest) canonicalRequest(names []string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r.Path))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.Query))
	b.WriteByte('\n')
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(r.headerValue(n))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(names, ";"))
	b.WriteByte('\n')
	sum := sha256.Sum256(r.Body)
	b.WriteString(hex.EncodeToString(sum[:]))
	return b.String()
}

// headerValue is the canonical value of the header with lower-case name n: every value in the
// order received, joined with commas, each trimmed and with runs of white space made one space.
func (r signRequest) headerValue(n string) string {
	if n == "host" {
		return collapseSpace(r.Host)
	}
	var vals []string
	for k, vs := range r.Header {
		if strings.ToLower(k) != n {
			continue
		}
		for _, v := range vs {
			vals = append(vals, collapseSpace(v))
		}
	}
	return strings.Join(vals, ",")
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

func (r signRequest) sign(creds Credentials, region, service string, t time.Time, names []string) signed {
	scope := strings.Join([]string{t.Format(dateFormat), region, service, "aws4_request"}, "/")
	creq := r.canonicalRequest(names)
	sum := sha256.Sum256([]byte(creq))
	sts := signAlgorithm + "\n" + t.Format(timeFormat) + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, t.Format(dateFormat), region, service), sts))
	signedHeaders := strings.Join(names, ";")
	return signed{
		CanonicalRequest: creq,
		StringToSign:     sts,
		SignedHeaders:    signedHeaders,
		Signature:        sig,
		Authorization: signAlgorithm + " Credential=" + creds.AccessKeyID + "/" + scope +
			", SignedHeaders=" + signedHeaders + ", Signature=" + sig,
	}
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// canonicalURI removes dot segments and repeated slashes from the path and encodes it. An empty
// result is "/", and a trailing slash survives.
func canonicalURI(p string) string {
	var out []string
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	last := segs[len(segs)-1]
	res := "/" + strings.Join(out, "/")
	if len(out) > 0 && (last == "" || last == "." || last == "..") {
		res += "/"
	}
	return uriEncode(res, true)
}

// canonicalQuery decodes, re-encodes and sorts the query parameters by encoded name, then value.
func canonicalQuery(raw string) string {
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, pair{uriEncode(queryUnescape(k), false), uriEncode(queryUnescape(v), false)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func queryUnescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

// uriEncode percent-encodes every byte except A-Z a-z 0-9 - _ . ~ (and "/" when keepSlash).
func uriEncode(s string, keepSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
	return b.String()
}
