package oauth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file holds the input rules of the authorization server: identifiers, client names, redirect
// URIs, web URLs and resource indicators. They are pure functions; the Service turns their failures
// into the error each endpoint answers with.

// ---- identifiers

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// canonUUID returns s in lower case when it is a well-formed UUID. An identifier that is not one is
// "not found" to the Store and to the Service, never a driver error.
func canonUUID(s string) (string, bool) {
	if !uuidShape.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// newUUID returns a random (version 4) UUID from crypto/rand: the form of every identifier the
// protocol exposes (app ids, client secret ids, auth_ids).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// orgSlugRE is the shape of the organization_slug parameter of an authorization request (\w and -).
var orgSlugRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ---- errors for callers that show a message

// invalidf is ErrInvalid with a text that is safe to show to the person who sent the input.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// limitf is ErrLimit with a text that says which cap was reached.
func limitf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLimit, fmt.Sprintf(format, args...))
}

// ErrorMessage is the text of an ErrInvalid or ErrLimit error from the Service without its sentinel
// prefix ("oauth: invalid request: "): the sentence the organization apps API shows. Any other error
// gives its own text, which is not meant for people.
func ErrorMessage(err error) string {
	msg := err.Error()
	for _, sentinel := range []error{ErrInvalid, ErrLimit} {
		msg = strings.TrimPrefix(msg, sentinel.Error()+": ")
	}
	return msg
}

// ---- client names

// cleanClientName makes a self-asserted name safe to show: whitespace of every kind becomes a single
// space; control characters, format characters (zero-width and bidirectional overrides can make
// "Evil" read as something else), private-use characters and U+FFFD are dropped. ok is false when
// the name is not valid UTF-8 or is empty or longer than MaxClientNameLen characters after cleaning.
// The result is always rendered as plain text.
func cleanClientName(s string) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			b.WriteByte(' ')
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), r == utf8.RuneError:
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	n := utf8.RuneCountInString(name)
	return name, n >= 1 && n <= MaxClientNameLen
}

// ---- redirect URIs

// loopbackHosts are the host names for which a redirect URI may use plain http (RFC 8252 section 7.3).
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

var hostnameRE = regexp.MustCompile(`^[a-z0-9._:-]+$`)

// hasControlOrSpace reports whether s has an ASCII control character, a space or DEL.
func hasControlOrSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// parseRedirectURI checks one redirect URI against the registration rules and returns it parsed. The
// error says what is wrong in words that are safe to show. The rules: at most MaxRedirectURILen
// bytes; no control characters, spaces, backslashes or fragment; no userinfo; a host in ASCII (an
// internationalized name must be registered in its punycode form, so that the consent page shows
// what the browser will visit); https, or http with the host localhost, 127.0.0.1 or [::1]. Custom
// schemes (cursor://, vscode://) are not accepted.
func parseRedirectURI(raw string) (*url.URL, error) {
	switch {
	case raw == "":
		return nil, errors.New("is empty")
	case len(raw) > MaxRedirectURILen:
		return nil, fmt.Errorf("is longer than %d characters", MaxRedirectURILen)
	case !utf8.ValidString(raw) || hasControlOrSpace(raw):
		return nil, errors.New("contains spaces or control characters")
	case strings.ContainsAny(raw, "#\\"):
		return nil, errors.New("must not contain a fragment or a backslash")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("is not a valid URL")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return nil, errors.New("is not an absolute URL with a host")
	}
	if u.User != nil {
		return nil, errors.New("must not contain a user name or password")
	}
	host := strings.ToLower(u.Hostname())
	if !hostnameRE.MatchString(host) {
		return nil, errors.New("has a host that is not plain ASCII (register an internationalized name in punycode)")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHosts[host] {
			return nil, errors.New("may use http for localhost, 127.0.0.1 and [::1] only")
		}
	default:
		return nil, errors.New("must use https (or http on localhost); custom schemes are not supported")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, errors.New("has an invalid port")
		}
	}
	return u, nil
}

// validateRedirectURIs checks a list of redirect URIs: 1 to MaxRedirectURIs entries, each valid. It
// returns the list without repeats, in the order given. The error is the sentence to show.
func validateRedirectURIs(uris []string) ([]string, error) {
	if len(uris) == 0 {
		return nil, errors.New("redirect_uris must list at least one URI")
	}
	if len(uris) > MaxRedirectURIs {
		return nil, fmt.Errorf("redirect_uris may list at most %d URIs", MaxRedirectURIs)
	}
	out := make([]string, 0, len(uris))
	for i, raw := range uris {
		if _, err := parseRedirectURI(raw); err != nil {
			return nil, fmt.Errorf("redirect_uris[%d] %w", i, err)
		}
		if !hasString(out, raw) {
			out = append(out, raw)
		}
	}
	return out, nil
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- web URLs (client_uri, logo_uri, website, icon)

// maxWebURLLen bounds client_uri, logo_uri, website and icon.
const maxWebURLLen = 2048

// checkWebURL checks a URL the server stores for display (and never fetches): at most maxWebURLLen
// bytes, no control characters or spaces, https (or http when httpsOnly is false), a host, no
// userinfo. The empty string is not checked; callers treat it as "none".
func checkWebURL(raw string, httpsOnly bool) error {
	switch {
	case len(raw) > maxWebURLLen:
		return fmt.Errorf("is longer than %d characters", maxWebURLLen)
	case !utf8.ValidString(raw) || hasControlOrSpace(raw):
		return errors.New("contains spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" {
		return errors.New("is not an absolute URL with a host")
	}
	if u.User != nil {
		return errors.New("must not contain a user name or password")
	}
	switch {
	case u.Scheme == "https", u.Scheme == "http" && !httpsOnly:
		return nil
	case httpsOnly:
		return errors.New("must be an https URL")
	}
	return errors.New("must be an http or https URL")
}

// ---- resource indicators (RFC 8707)

// canonicalResource returns the canonical form of an absolute http(s) URL used as a resource
// indicator: scheme and host in lower case, no query, no fragment, and no trailing slash. ok is
// false for anything else.
func canonicalResource(raw string) (string, bool) {
	if raw == "" || len(raw) > maxWebURLLen || !utf8.ValidString(raw) || hasControlOrSpace(raw) {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/"), true
}

// resolveResource maps a resource indicator of a request to this server's own spelling of it:
// "<issuer>/mcp" or "<issuer>". ok is false when the request names another resource. Grants store
// the server's spelling, so that comparing a grant's resource with oauth.ResourceURL(issuer) is a
// string comparison.
func (s *Service) resolveResource(raw string) (string, bool) {
	want, ok := canonicalResource(raw)
	if !ok {
		return "", false
	}
	for _, own := range []string{ResourceURL(s.Issuer), strings.TrimRight(s.Issuer, "/")} {
		if c, ok := canonicalResource(own); ok && c == want {
			return own, true
		}
	}
	return "", false
}
