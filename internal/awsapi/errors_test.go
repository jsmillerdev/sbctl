package awsapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/awsapi/awsfake"
)

// Every way to reach the metadata service keeps the cause in the chain, so a caller can tell a
// service that is switched off, or a call that was cancelled, from a failure.
func TestIMDSErrorsWrapTheirCause(t *testing.T) {
	fake := awsfake.New(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	for name, tc := range map[string]struct {
		env  map[string]string
		ctx  context.Context
		want error
	}{
		"disabled":  {map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}, ctx, awsapi.ErrIMDSDisabled},
		"cancelled": {nil, cancelled, context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := fake.Config()
			cfg.Getenv = envOf(tc.env)
			withRegion := newClient(t, cfg)
			cfg.Region = ""
			noRegion := newClient(t, cfg)

			_, region := noRegion.Region(tc.ctx)
			_, call := withRegion.EC2.DescribeInstances(tc.ctx, awsapi.DescribeInstancesInput{})
			_, role := awsapi.IMDSCredentials(withRegion.IMDS).Retrieve(tc.ctx)
			_, read := withRegion.IMDS.InstanceID(tc.ctx)
			_, tags := withRegion.IMDS.Tags(tc.ctx)
			for what, err := range map[string]error{"Region": region, "an EC2 call": call, "IMDSCredentials": role, "InstanceID": read, "Tags": tags} {
				if !errors.Is(err, tc.want) {
					t.Errorf("%s: %v does not wrap %v", what, err, tc.want)
				}
			}
			// The credential paths still say there were no credentials to be had.
			for what, err := range map[string]error{"an EC2 call": call, "IMDSCredentials": role} {
				if !errors.Is(err, awsapi.ErrNoCredentials) {
					t.Errorf("%s: %v does not wrap ErrNoCredentials", what, err)
				}
			}
		})
	}
}

// A wait between tries that ends with the context returns the last failure and the context's error.
func TestRetryWaitEndsWithBothErrors(t *testing.T) {
	t.Run("api", func(t *testing.T) {
		cfg, _ := stub(t, answer{status: 503, body: `<Response><Errors><Error><Code>RequestLimitExceeded</Code><Message>slow down</Message></Error></Errors></Response>`})
		cfg.RetryBackoff = time.Hour
		cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := newClient(t, cfg).EC2.StopInstances(cctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-1"}})
		if !errors.Is(err, context.DeadlineExceeded) || !awsapi.IsCode(err, "RequestLimitExceeded") || !strings.HasPrefix(err.Error(), "aws ec2 StopInstances: RequestLimitExceeded") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("metadata service", func(t *testing.T) {
		fake := awsfake.New(t)
		fake.Inject("imds", "/latest/meta-data/instance-id", awsfake.Fault{Status: 503})
		cfg := fake.Config()
		cfg.RetryBackoff = time.Hour
		cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := newClient(t, cfg).IMDS.InstanceID(cctx)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), "aws imds /latest/meta-data/instance-id: status 503") {
			t.Errorf("err = %v", err)
		}
	})
}

// An error says first which call or read failed: "aws <service> <action>:" for the services,
// "aws imds <path>:" for the metadata service, "awsapi:" for the client's own setup.
func TestErrorsNameWhatFailed(t *testing.T) {
	fake := awsfake.New(t)
	disabled := fake.Config()
	disabled.Getenv = envOf(map[string]string{"AWS_EC2_METADATA_DISABLED": "true"})
	noRegion := disabled
	noRegion.Region = ""
	unreadable, _ := stub(t, xmlOK("<not xml"))

	noRole := awsfake.New(t)
	d := noRole.IMDS()
	d.Role = ""
	noRole.SetIMDS(d)

	refused := awsfake.New(t)
	refused.Inject("imds", "/latest/api/token", awsfake.Fault{Status: 403})

	_, noCreds := newClient(t, disabled).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	_, noRegionCall := newClient(t, noRegion).STS.AssumeRole(ctx, awsapi.AssumeRoleInput{RoleARN: "arn", SessionName: "s"})
	_, noRegionErr := newClient(t, noRegion).Region(ctx)
	_, unreadableErr := newClient(t, unreadable).EC2.DescribeInstances(ctx, awsapi.DescribeInstancesInput{})
	_, apiErr := fake.Client().EC2.StopInstances(ctx, awsapi.StopInstancesInput{InstanceIDs: []string{"i-0missing"}})
	_, secretErr := fake.Client().SecretsManager.GetSecretValue(ctx, awsapi.GetSecretValueInput{SecretID: "missing"})
	_, disabledRead := newClient(t, disabled).IMDS.InstanceID(ctx)
	_, missingRead := fake.Client().IMDS.Get(ctx, "/latest/meta-data/nothing-here")
	_, noRoleErr := noRole.Client().IMDS.RoleCredentials(ctx)
	_, refusedRead := refused.Client().IMDS.InstanceID(ctx)
	_, badEndpoint := awsapi.New(awsapi.Config{Endpoints: awsapi.Endpoints{EC2: "nope"}})

	for _, tc := range []struct {
		name   string
		err    error
		prefix string
	}{
		{"no credentials", noCreds, "aws ec2 DescribeInstances: no AWS credentials: "},
		{"no region in a call", noRegionCall, "aws sts AssumeRole: no region: "},
		{"no region", noRegionErr, "awsapi: no region: "},
		{"unreadable reply", unreadableErr, "aws ec2 DescribeInstances: unreadable response"},
		{"API error", apiErr, "aws ec2 StopInstances: InvalidInstanceID.NotFound"},
		{"secret error", secretErr, "aws secretsmanager GetSecretValue: ResourceNotFoundException"},
		{"disabled read", disabledRead, "aws imds /latest/meta-data/instance-id: "},
		{"missing path", missingRead, "aws imds /latest/meta-data/nothing-here: "},
		{"no role", noRoleErr, "aws imds role credentials: "},
		{"refused token", refusedRead, "aws imds token: "},
		{"bad endpoint", badEndpoint, "awsapi: "},
	} {
		if tc.err == nil || !strings.HasPrefix(tc.err.Error(), tc.prefix) {
			t.Errorf("%s: %v, want a prefix %q", tc.name, tc.err, tc.prefix)
		}
	}
}
