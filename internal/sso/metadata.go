package sso

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// MaxMetadataBytes bounds a metadata document (real ones are a few kilobytes).
const MaxMetadataBytes = 2 << 20

// domainRe is the domain syntax of the Management API's SSO domains.
var domainRe = regexp.MustCompile(`^([a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// NormalizeDomain lower-cases an email domain and checks its syntax ("@acme.com" is accepted).
func NormalizeDomain(d string) (string, error) {
	d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
	if !domainRe.MatchString(d) {
		return "", fmt.Errorf("%q is not an email domain such as acme.com", d)
	}
	return d, nil
}

// NormalizeDomains normalizes a list, refusing duplicates.
func NormalizeDomains(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, d := range in {
		n, err := NormalizeDomain(d)
		if err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, fmt.Errorf("domain %s is listed twice", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// Metadata is where an identity provider's SAML metadata comes from.
type Metadata struct {
	// URL is an https address GoTrue fetches itself, now and again when the document says it
	// is stale; a plain-http address on this machine (a development IdP) is fetched here instead.
	URL string
	// XML is the document itself.
	XML string
}

// ValidateXML checks that doc is a SAML metadata document with the entity id and the one
// IDPSSODescriptor GoTrue needs, and returns the entity id.
func ValidateXML(doc string) (entityID string, err error) {
	if len(doc) > MaxMetadataBytes {
		return "", errors.New("the metadata document is larger than 2 MB")
	}
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.Strict = true
	idp := 0
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("the metadata is not well-formed XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 1:
				if t.Name.Local != "EntityDescriptor" {
					return "", fmt.Errorf("the metadata's root element is %s, not EntityDescriptor", t.Name.Local)
				}
				for _, a := range t.Attr {
					if a.Name.Local == "entityID" {
						entityID = a.Value
					}
				}
			case depth == 2 && t.Name.Local == "IDPSSODescriptor":
				idp++
			}
		case xml.EndElement:
			depth--
		}
	}
	switch {
	case entityID == "":
		return "", errors.New("the metadata has no entityID")
	case idp == 0:
		return "", errors.New("the metadata has no IDPSSODescriptor: it describes a service provider, not an identity provider")
	case idp > 1:
		return "", errors.New("the metadata has several IDPSSODescriptors, which GoTrue does not support")
	}
	return entityID, nil
}

// IsLoopbackURL reports whether the host of u is this machine.
func IsLoopbackURL(u *url.URL) bool {
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ResolveMetadata turns what an administrator gave (an address or a file) into what GoTrue is
// given. An https address is passed on: GoTrue fetches it and keeps refreshing it. A file is
// read and validated. A plain http address is refused unless it is on this machine, because
// anyone on the path could replace the identity provider's signing certificate and sign in as
// anybody; a local one (a development IdP) is fetched here and passed as the document, which
// GoTrue does not refresh.
func ResolveMetadata(ctx context.Context, urlStr, file string, hc *http.Client) (Metadata, error) {
	switch {
	case urlStr != "" && file != "":
		return Metadata{}, errors.New("give the metadata as an address or as a file, not both")
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return Metadata{}, fmt.Errorf("read the metadata file: %w", err)
		}
		if _, err := ValidateXML(string(b)); err != nil {
			return Metadata{}, err
		}
		return Metadata{XML: string(b)}, nil
	case urlStr == "":
		return Metadata{}, errors.New("the identity provider's metadata is needed: an https address or a file")
	}
	u, err := url.Parse(urlStr)
	if err != nil || u.Host == "" {
		return Metadata{}, fmt.Errorf("%q is not a URL", urlStr)
	}
	switch {
	case u.Scheme == "https":
		return Metadata{URL: urlStr}, nil
	case u.Scheme == "http" && IsLoopbackURL(u):
		if hc == nil {
			hc = &http.Client{Timeout: 30 * time.Second}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if err != nil {
			return Metadata{}, err
		}
		req.Header.Set("Accept", "application/xml;charset=UTF-8")
		resp, err := hc.Do(req)
		if err != nil {
			return Metadata{}, fmt.Errorf("fetch the metadata: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return Metadata{}, fmt.Errorf("fetching the metadata answered HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, MaxMetadataBytes+1))
		if err != nil {
			return Metadata{}, err
		}
		if _, err := ValidateXML(string(b)); err != nil {
			return Metadata{}, err
		}
		return Metadata{XML: string(b)}, nil
	}
	return Metadata{}, errors.New("the metadata address must be https (a plain http address is accepted only on this machine); save the document and pass it as a file otherwise")
}

// SPURLs are what an identity provider is configured with: where it sends the signed
// assertion, the entity id it addresses it to, and the service provider metadata that holds both
// and the signing certificate.
type SPURLs struct {
	EntityID    string `json:"entity_id"`
	ACSURL      string `json:"acs_url"`
	MetadataURL string `json:"metadata_url"`
}

// URLsFor derives the service-provider endpoints of the GoTrue whose API_EXTERNAL_URL is
// externalURL (https://<host>/auth/v1): GoTrue serves them under <external>/sso/saml. The
// metadata address asks for the download form, which GoTrue gives a validity of five years
// (the plain document is valid for two days, which an identity provider that imports a file
// takes literally).
func URLsFor(externalURL string) SPURLs {
	base := strings.TrimRight(externalURL, "/")
	return SPURLs{
		EntityID:    base + "/sso/saml/metadata",
		ACSURL:      base + "/sso/saml/acs",
		MetadataURL: base + "/sso/saml/metadata?download=true",
	}
}
