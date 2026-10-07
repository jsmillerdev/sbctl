package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls the SSO provider endpoints of one GoTrue's admin API
// (/admin/sso/providers, supabase/auth internal/api/ssoadmin.go) with its service_role key.
type Client struct {
	// BaseURL is the loopback address of the GoTrue, with no trailing slash.
	BaseURL    string
	ServiceKey string
	HTTP       *http.Client
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// APIError is an error answer of GoTrue.
type APIError struct {
	Status  int
	Code    string // GoTrue's error_code, for example sso_provider_not_found
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("gotrue answered %d", e.Status)
	}
	return fmt.Sprintf("gotrue answered %d: %s", e.Status, e.Message)
}

// Attribute maps one claim of the user from the identity provider's assertion.
type Attribute struct {
	Name    string   `json:"name,omitempty"`
	Names   []string `json:"names,omitempty"`
	Array   bool     `json:"array,omitempty"`
	Default any      `json:"default,omitempty"`
}

// AttributeMapping is GoTrue's attribute_mapping: claim name to attribute.
type AttributeMapping struct {
	Keys map[string]Attribute `json:"keys"`
}

// SAML is the SAML half of a provider.
type SAML struct {
	EntityID         string            `json:"entity_id"`
	MetadataURL      string            `json:"metadata_url,omitempty"`
	MetadataXML      string            `json:"metadata_xml,omitempty"`
	AttributeMapping *AttributeMapping `json:"attribute_mapping,omitempty"`
	NameIDFormat     string            `json:"name_id_format,omitempty"`
}

// Domain is an email domain a provider serves.
type Domain struct {
	Domain    string `json:"domain"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Provider is a SAML identity provider in the shape of the Management API
// (CreateProviderResponse, GetProviderResponse, ...), which is GoTrue's own minus its extras.
type Provider struct {
	ID        string   `json:"id"`
	SAML      *SAML    `json:"saml,omitempty"`
	Domains   []Domain `json:"domains"`
	CreatedAt string   `json:"created_at,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// DomainNames lists the domains of p, lower case.
func (p *Provider) DomainNames() []string {
	out := make([]string, 0, len(p.Domains))
	for _, d := range p.Domains {
		out = append(out, strings.ToLower(d.Domain))
	}
	return out
}

// Normalize makes p safe to encode as the spec's answer: domains and the attribute mapping's
// keys are present even when empty (the specs require attribute_mapping.keys, and GoTrue
// answers an empty mapping as {}).
func (p *Provider) Normalize() *Provider {
	if p.Domains == nil {
		p.Domains = []Domain{}
	}
	if p.SAML != nil {
		if p.SAML.AttributeMapping == nil {
			p.SAML.AttributeMapping = &AttributeMapping{}
		}
		if p.SAML.AttributeMapping.Keys == nil {
			p.SAML.AttributeMapping.Keys = map[string]Attribute{}
		}
	}
	return p
}

// CreateBody is the body of POST /admin/sso/providers (the Management API's CreateProviderBody).
type CreateBody struct {
	Type             string            `json:"type"`
	MetadataURL      string            `json:"metadata_url,omitempty"`
	MetadataXML      string            `json:"metadata_xml,omitempty"`
	Domains          []string          `json:"domains,omitempty"`
	AttributeMapping *AttributeMapping `json:"attribute_mapping,omitempty"`
	NameIDFormat     string            `json:"name_id_format,omitempty"`
}

// UpdateBody is the body of PUT /admin/sso/providers/{id}. A nil Domains leaves the domains
// alone; an empty one removes them all (GoTrue tells the two apart).
type UpdateBody struct {
	MetadataURL      string            `json:"metadata_url,omitempty"`
	MetadataXML      string            `json:"metadata_xml,omitempty"`
	Domains          *[]string         `json:"domains,omitempty"`
	AttributeMapping *AttributeMapping `json:"attribute_mapping,omitempty"`
	NameIDFormat     *string           `json:"name_id_format,omitempty"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.ServiceKey)
	req.Header.Set("apikey", c.ServiceKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("sso: GoTrue is not reachable: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Msg       string `json:"msg"`
			Message   string `json:"message"`
			ErrorCode string `json:"error_code"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Msg
		if msg == "" {
			msg = e.Message
		}
		return &APIError{Status: resp.StatusCode, Code: e.ErrorCode, Message: msg}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("sso: decode GoTrue's answer to %s %s: %w", method, path, err)
		}
	}
	return nil
}

// List returns every provider. GoTrue leaves the metadata XML out of the list; Get has it.
func (c *Client) List(ctx context.Context) ([]*Provider, error) {
	var res struct {
		Items []*Provider `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/admin/sso/providers", nil, &res); err != nil {
		return nil, err
	}
	for _, p := range res.Items {
		p.Normalize()
	}
	if res.Items == nil {
		res.Items = []*Provider{}
	}
	return res.Items, nil
}

// Create registers a provider.
func (c *Client) Create(ctx context.Context, b CreateBody) (*Provider, error) {
	b.Type = "saml"
	var p Provider
	if err := c.do(ctx, http.MethodPost, "/admin/sso/providers", b, &p); err != nil {
		return nil, err
	}
	return p.Normalize(), nil
}

// Get returns one provider; id is a GoTrue provider id.
func (c *Client) Get(ctx context.Context, id string) (*Provider, error) {
	var p Provider
	if err := c.do(ctx, http.MethodGet, "/admin/sso/providers/"+url.PathEscape(id), nil, &p); err != nil {
		return nil, err
	}
	return p.Normalize(), nil
}

// Update changes a provider.
func (c *Client) Update(ctx context.Context, id string, b UpdateBody) (*Provider, error) {
	var p Provider
	if err := c.do(ctx, http.MethodPut, "/admin/sso/providers/"+url.PathEscape(id), b, &p); err != nil {
		return nil, err
	}
	return p.Normalize(), nil
}

// Delete removes a provider and returns what was removed.
func (c *Client) Delete(ctx context.Context, id string) (*Provider, error) {
	var p Provider
	if err := c.do(ctx, http.MethodDelete, "/admin/sso/providers/"+url.PathEscape(id), nil, &p); err != nil {
		return nil, err
	}
	return p.Normalize(), nil
}
