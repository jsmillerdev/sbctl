package awsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// SecretsManager reads secrets. It speaks the awsJson1.1 protocol, not the Query API.
type SecretsManager struct{ c *core }

// GetSecretValueInput names a secret by name or ARN. Without VersionID or VersionStage it asks
// for AWSCURRENT.
type GetSecretValueInput struct {
	SecretID     string
	VersionID    string
	VersionStage string
}

// SecretValue is a secret's contents. A secret holds either a string or bytes. Printing one does
// not show the contents.
type SecretValue struct {
	ARN           string
	Name          string
	VersionID     string
	SecretString  string
	SecretBinary  []byte
	VersionStages []string
}

func (s SecretValue) String() string   { return "SecretValue{" + s.Name + "}" }
func (s SecretValue) GoString() string { return s.String() }

// GetSecretValue returns the secret. A secret that does not exist is an *Error for which
// IsNotFound is true; one the role may not read has Code AccessDeniedException (IsAccessDenied).
func (s *SecretsManager) GetSecretValue(ctx context.Context, in GetSecretValueInput) (SecretValue, error) {
	req := map[string]string{"SecretId": in.SecretID}
	if in.VersionID != "" {
		req["VersionId"] = in.VersionID
	}
	if in.VersionStage != "" {
		req["VersionStage"] = in.VersionStage
	}
	body, _ := json.Marshal(req)
	const action = "GetSecretValue"
	b, err := s.c.do(ctx, apiCall{
		service: "secretsmanager", host: "secretsmanager", action: action,
		header: http.Header{
			"Content-Type": {"application/x-amz-json-1.1"},
			"X-Amz-Target": {"secretsmanager." + action},
		},
		body:     body,
		parseErr: parseJSONError,
	})
	if err != nil {
		return SecretValue{}, err
	}
	var resp struct {
		ARN           string `json:"ARN"`
		Name          string
		VersionID     string `json:"VersionId"`
		SecretString  string
		SecretBinary  []byte
		VersionStages []string
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return SecretValue{}, fmt.Errorf("aws secretsmanager %s: unreadable response: %w", action, err)
	}
	return SecretValue(resp), nil
}
