package awsfake

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
)

// IMDSData is what the fake metadata service answers about the instance it pretends to be.
type IMDSData struct {
	InstanceID string
	Region     string
	AZ         string
	PublicIP   string // "" answers 404, like an instance without a public address
	LocalIP    string
	Tags       map[string]string
	// TagsHidden answers 404 for the tags, like an instance without InstanceMetadataTags.
	TagsHidden bool
	// Role is the instance role's name; "" means the instance has none.
	Role string
	// CredentialsTTL is how long the role credentials are valid, counted from each read.
	CredentialsTTL time.Duration
}

// IMDS returns the data the metadata service answers with.
func (s *Server) IMDS() IMDSData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.imds
}

// SetIMDS replaces the data the metadata service answers with.
func (s *Server) SetIMDS(d IMDSData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imds = d
}

func (s *Server) serveIMDS(w http.ResponseWriter, r *http.Request) {
	call := Call{Service: "imds", Action: r.Method + " " + r.URL.Path, Params: url.Values{}}
	status, body := s.imdsAnswer(r)
	s.record(&call, result{status: status})
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (s *Server) imdsAnswer(r *http.Request) (int, string) {
	if f, hit := s.injected("imds", r.URL.Path); hit {
		return f.status, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/latest/api/token" {
		if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			return http.StatusBadRequest, ""
		}
		tok := fmt.Sprintf("fake-imds-token-%d", s.next())
		s.imdsTokens[tok] = true
		return http.StatusOK, tok
	}
	if r.Method != http.MethodGet || !s.imdsTokens[r.Header.Get("X-aws-ec2-metadata-token")] {
		return http.StatusUnauthorized, ""
	}
	d := s.imds
	const meta = "/latest/meta-data/"
	switch p := r.URL.Path; {
	case p == meta+"instance-id":
		return http.StatusOK, d.InstanceID
	case p == meta+"placement/region":
		return http.StatusOK, d.Region
	case p == meta+"placement/availability-zone":
		return http.StatusOK, d.AZ
	case p == meta+"local-ipv4":
		return http.StatusOK, d.LocalIP
	case p == meta+"public-ipv4" && d.PublicIP != "":
		return http.StatusOK, d.PublicIP
	case p == meta+"tags/instance" && !d.TagsHidden && len(d.Tags) > 0:
		keys := make([]string, 0, len(d.Tags))
		for k := range d.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return http.StatusOK, strings.Join(keys, "\n")
	case strings.HasPrefix(p, meta+"tags/instance/") && !d.TagsHidden:
		if v, found := d.Tags[strings.TrimPrefix(p, meta+"tags/instance/")]; found {
			return http.StatusOK, v
		}
	case p == meta+"iam/security-credentials/" && d.Role != "":
		return http.StatusOK, d.Role
	case d.Role != "" && p == meta+"iam/security-credentials/"+d.Role:
		c := roleCredentials()
		doc, _ := json.Marshal(map[string]string{
			"Code": "Success", "Type": "AWS-HMAC", "AccessKeyId": c.AccessKeyID, "SecretAccessKey": c.SecretAccessKey,
			"Token": c.SessionToken, "Expiration": time.Now().Add(d.CredentialsTTL).UTC().Format(time.RFC3339),
		})
		return http.StatusOK, string(doc)
	}
	return http.StatusNotFound, ""
}

// Secret is a Secrets Manager secret in the fake. Give String or Binary.
type Secret struct {
	Name      string
	ARN       string // arn:aws:secretsmanager:<region>:123456789012:secret:<name>-AbCdEf when empty
	VersionID string
	String    string
	Binary    []byte
}

// AddSecret adds or replaces a secret.
func (s *Server) AddSecret(sec Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec.ARN == "" {
		sec.ARN = "arn:aws:secretsmanager:" + s.imds.Region + ":123456789012:secret:" + sec.Name + "-AbCdEf"
	}
	if sec.VersionID == "" {
		sec.VersionID = "11111111-2222-3333-4444-555555555555"
	}
	for i, o := range s.secrets {
		if o.Name == sec.Name {
			s.secrets[i] = sec
			return
		}
	}
	s.secrets = append(s.secrets, sec)
}

func jsonParams(body []byte) url.Values {
	q := url.Values{}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		for k, v := range m {
			if str, isStr := v.(string); isStr {
				q.Set(k, str)
			}
		}
	}
	return q
}

func (s *Server) secretsManager(c Call) result {
	if r, hit := s.injected("secretsmanager", c.Action); hit {
		return r
	}
	if c.Action != "GetSecretValue" {
		return fail(http.StatusBadRequest, "UnknownOperationException", c.Action)
	}
	id := c.Params.Get("SecretId")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sec := range s.secrets {
		if sec.Name != id && sec.ARN != id {
			continue
		}
		out := map[string]any{"ARN": sec.ARN, "Name": sec.Name, "VersionId": sec.VersionID, "VersionStages": []string{"AWSCURRENT"}, "CreatedDate": 1.7e9}
		if sec.Binary != nil {
			out["SecretBinary"] = base64.StdEncoding.EncodeToString(sec.Binary)
		} else {
			out["SecretString"] = sec.String
		}
		b, _ := json.Marshal(out)
		return success(string(b))
	}
	return fail(http.StatusBadRequest, "ResourceNotFoundException", "Secrets Manager can't find the specified secret.")
}

func (s *Server) sts(c Call) result {
	if r, hit := s.injected("sts", c.Action); hit {
		return r
	}
	if c.Action != "AssumeRole" {
		return fail(http.StatusBadRequest, "InvalidAction", c.Action)
	}
	arn := c.Params.Get("RoleArn")
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.roles[arn] {
		return fail(http.StatusForbidden, "AccessDenied", "User is not authorized to perform: sts:AssumeRole on resource: "+arn)
	}
	secs := 3600
	if d := c.Params.Get("DurationSeconds"); d != "" {
		secs, _ = strconv.Atoi(d)
	}
	n := s.next()
	creds := awsapi.Credentials{
		AccessKeyID: fmt.Sprintf("ASIAFAKEASSUMED%04d", n), SecretAccessKey: fmt.Sprintf("fake-assumed-secret-%d", n), SessionToken: fmt.Sprintf("fake-assumed-token-%d", n),
	}
	s.creds[creds.AccessKeyID] = creds
	name := arn[strings.LastIndexByte(arn, '/')+1:]
	return success(fmt.Sprintf(`<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey><SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials><AssumedRoleUser><AssumedRoleId>AROAFAKE:%s</AssumedRoleId><Arn>arn:aws:sts::123456789012:assumed-role/%s/%s</Arn></AssumedRoleUser></AssumeRoleResult><ResponseMetadata><RequestId>fake-request</RequestId></ResponseMetadata></AssumeRoleResponse>`,
		creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, time.Now().Add(time.Duration(secs)*time.Second).UTC().Format(time.RFC3339),
		esc(c.Params.Get("RoleSessionName")), esc(name), esc(c.Params.Get("RoleSessionName"))))
}
