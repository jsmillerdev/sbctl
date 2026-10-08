package awsapi

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const stsVersion = "2011-06-15"

// STS assumes roles.
type STS struct{ c *core }

// AssumeRoleInput names the role to assume with the client's own credentials.
type AssumeRoleInput struct {
	RoleARN     string
	SessionName string
	// Duration is how long the credentials last: 15 minutes to the role's maximum, 1 hour by default.
	Duration   time.Duration
	ExternalID string
}

// AssumedRole is the result of AssumeRole.
type AssumedRole struct {
	Credentials Credentials
	// ARN is the ARN of the assumed-role session.
	ARN string
}

// AssumeRole returns temporary credentials for a role.
func (s *STS) AssumeRole(ctx context.Context, in AssumeRoleInput) (AssumedRole, error) {
	p := params{}
	p.set("Action", "AssumeRole")
	p.set("Version", stsVersion)
	p.set("RoleArn", in.RoleARN)
	p.set("RoleSessionName", in.SessionName)
	if in.Duration > 0 {
		p.set("DurationSeconds", strconv.Itoa(int(in.Duration/time.Second)))
	}
	p.set("ExternalId", in.ExternalID)
	body, err := s.c.do(ctx, apiCall{
		service: "sts", host: "sts", action: "AssumeRole",
		header:   http.Header{"Content-Type": {contentForm}},
		body:     p.encode(),
		parseErr: func(status int, _ http.Header, b []byte) *Error { return parseXMLError(status, b) },
	})
	if err != nil {
		return AssumedRole{}, err
	}
	var resp struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"AssumeRoleResult>Credentials"`
		ARN string `xml:"AssumeRoleResult>AssumedRoleUser>Arn"`
	}
	if err := xml.Unmarshal(body, &resp); err != nil {
		return AssumedRole{}, fmt.Errorf("aws sts AssumeRole: unreadable response: %w", err)
	}
	c := resp.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return AssumedRole{}, fmt.Errorf("aws sts AssumeRole: the response has no credentials")
	}
	return AssumedRole{
		Credentials: Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Expires: parseTime(c.Expiration)},
		ARN:         resp.ARN,
	}, nil
}

// RoleCredentials is a provider that assumes the role and renews the credentials shortly before
// they expire. Hand it to another client's Config.Credentials to call AWS as the role.
func (s *STS) RoleCredentials(in AssumeRoleInput) CredentialProvider {
	return CachedCredentials(CredentialProviderFunc(func(ctx context.Context) (Credentials, error) {
		r, err := s.AssumeRole(ctx, in)
		return r.Credentials, err
	}), s.c.cfg.Now)
}
