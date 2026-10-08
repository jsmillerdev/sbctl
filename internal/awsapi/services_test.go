package awsapi_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

func jsonOK(body string) answer { return answer{status: 200, body: body} }

func TestGetSecretValueRequestAndReply(t *testing.T) {
	cfg, seen := stub(t, jsonOK(fixture(t, "secret_value.json")), jsonOK(fixture(t, "secret_binary.json")))
	c := newClient(t, cfg)
	got, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "supavise/join", VersionStage: "AWSCURRENT"})
	if err != nil {
		t.Fatal(err)
	}
	want := awsapi.SecretValue{
		ARN: "arn:aws:secretsmanager:us-east-1:123456789012:secret:supavise/join-AbCdEf", Name: "supavise/join",
		VersionID: "EXAMPLE1-90ab-cdef-fedc-ba987EXAMPLE", SecretString: `{"token":"t0k3n"}`, VersionStages: []string{"AWSCURRENT"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secret\n got: %+v\nwant: %+v", got, want)
	}
	bin, err := c.SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "blob", VersionID: "v1"})
	if err != nil || !reflect.DeepEqual(bin.SecretBinary, []byte{0, 1, 2, 255}) || bin.SecretString != "" || len(bin.VersionStages) != 2 {
		t.Errorf("binary secret: %+v, %v", bin, err)
	}

	reqs := seen()
	if reqs[0].Body != `{"SecretId":"supavise/join","VersionStage":"AWSCURRENT"}` || reqs[1].Body != `{"SecretId":"blob","VersionId":"v1"}` {
		t.Errorf("bodies: %s | %s", reqs[0].Body, reqs[1].Body)
	}
	h := reqs[0].Header
	if h.Get("Content-Type") != "application/x-amz-json-1.1" || h.Get("X-Amz-Target") != "secretsmanager.GetSecretValue" {
		t.Errorf("headers: %v", h)
	}
	if !strings.Contains(h.Get("Authorization"), "/us-east-1/secretsmanager/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-target,") {
		t.Errorf("Authorization = %s", h.Get("Authorization"))
	}
}

func TestSecretValuePrintsWithoutTheContents(t *testing.T) {
	v := awsapi.SecretValue{Name: "supavise/join", SecretString: "t0k3n"}
	if s := fmt.Sprintf("%v %+v %#v %s", v, v, v, v); strings.Contains(s, "t0k3n") {
		t.Errorf("printed: %s", s)
	}
}

func TestSecretsManagerErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		ans      answer
		code     string
		notFound bool
		denied   bool
	}{
		"type field": {answer{status: 400, body: `{"__type":"ResourceNotFoundException","Message":"Secrets Manager can't find the specified secret."}`}, "ResourceNotFoundException", true, false},
		"namespaced type and header": {answer{status: 400, body: `{"__type":"com.amazonaws.secretsmanager#ResourceNotFoundException","message":"gone"}`,
			header: map[string]string{"X-Amzn-ErrorType": "ResourceNotFoundException:http://internal.amazon.com/coral/com.amazonaws.secretsmanager/", "X-Amzn-RequestId": "req-1"}}, "ResourceNotFoundException", true, false},
		"access denied": {answer{status: 400, body: `{"__type":"AccessDeniedException","Message":"not allowed"}`}, "AccessDeniedException", false, true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, seen := stub(t, tc.ans)
			_, err := newClient(t, cfg).SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "x"})
			if !awsapi.IsCode(err, tc.code) || awsapi.IsNotFound(err) != tc.notFound || awsapi.IsAccessDenied(err) != tc.denied {
				t.Errorf("%v", err)
			}
			if len(seen()) != 1 {
				t.Errorf("%d requests", len(seen()))
			}
		})
	}
	// A server error is retried.
	cfg, seen := stub(t, answer{status: 500, body: `{"__type":"InternalServiceError","Message":"x"}`}, jsonOK(fixture(t, "secret_value.json")))
	if _, err := newClient(t, cfg).SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "x"}); err != nil || len(seen()) != 2 {
		t.Errorf("500 then success: %v, %d requests", err, len(seen()))
	}
}

func TestAssumeRoleRequestAndReply(t *testing.T) {
	cfg, seen := stub(t, xmlOK(fixture(t, "assume_role.xml")), answer{status: 403, body: fixture(t, "sts_error.xml")})
	c := newClient(t, cfg)
	got, err := c.STS.AssumeRole(ctx, awsapi.AssumeRoleInput{
		RoleARN: "arn:aws:iam::123456789012:role/demo", SessionName: "TestAR", Duration: 15 * time.Minute, ExternalID: "ext 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Credentials.AccessKeyID != "ASIAIOSFODNN7EXAMPLE" || got.Credentials.SecretAccessKey != "wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLEKEY" ||
		!strings.HasPrefix(got.Credentials.SessionToken, "AQoDYXdz") || !got.Credentials.Expires.Equal(time.Date(2026, 10, 8, 13, 34, 41, 0, time.UTC)) ||
		got.ARN != "arn:aws:sts::123456789012:assumed-role/demo/TestAR" {
		t.Errorf("%+v", got)
	}
	want := "Action=AssumeRole&DurationSeconds=900&ExternalId=ext%201&RoleArn=arn%3Aaws%3Aiam%3A%3A123456789012%3Arole%2Fdemo&RoleSessionName=TestAR&Version=2011-06-15"
	if b := seen()[0].Body; b != want {
		t.Errorf("body\n got: %s\nwant: %s", b, want)
	}
	if a := seen()[0].Header.Get("Authorization"); !strings.Contains(a, "/us-east-1/sts/aws4_request") {
		t.Errorf("Authorization = %s", a)
	}

	_, err = c.STS.AssumeRole(ctx, awsapi.AssumeRoleInput{RoleARN: "arn:aws:iam::123456789012:role/other", SessionName: "s"})
	var e *awsapi.Error
	if !asError(err, &e) || e.Service != "sts" || e.Code != "AccessDenied" || e.RequestID != "4c1f6d2e-aaaa-bbbb-cccc-example" || !awsapi.IsAccessDenied(err) {
		t.Errorf("denied: %v", err)
	}
}

func TestAssumedRoleCredentialsSignLaterCalls(t *testing.T) {
	fake := awsfake.New(t)
	const role = "arn:aws:iam::123456789012:role/storage"
	fake.AddRole(role)
	fake.AddInstance(awsfake.Instance{ID: "i-1"})

	base := fake.Client()
	cfg := fake.Config()
	cfg.Credentials = base.STS.RoleCredentials(awsapi.AssumeRoleInput{RoleARN: role, SessionName: "supavise-storage"})
	assumed := newClient(t, cfg)
	for i := 0; i < 2; i++ {
		if _, err := assumed.EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{}); err != nil {
			t.Fatal(err)
		}
	}
	var keys []string
	for _, c := range fake.Calls() {
		if c.Service == "ec2" || c.Service == "sts" {
			keys = append(keys, c.Service+":"+c.AccessKeyID[:12])
		}
	}
	// One AssumeRole signed by the instance role, then both EC2 calls signed by the assumed role.
	want := []string{"sts:ASIAFAKEROLE", "ec2:ASIAFAKEASSU", "ec2:ASIAFAKEASSU"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("calls %v, want %v", keys, want)
	}

	if _, err := base.STS.AssumeRole(ctx, awsapi.AssumeRoleInput{RoleARN: "arn:aws:iam::123456789012:role/unknown", SessionName: "x"}); !awsapi.IsAccessDenied(err) {
		t.Errorf("unregistered role: %v", err)
	}
}
