package main

import (
	"context"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/awsapi"
	"github.com/supavise/supavise/internal/infra"
)

// A cloud that does not answer holds nothing up: the plan of an upgrade, `supavise status` and an
// emergency `supavise rollback` ask the question on a machine that only looks like EC2.
func TestTheQuestionToTheCloudIsBounded(t *testing.T) {
	t.Setenv(awsapi.EnvEndpointIMDS, "http://127.0.0.1:1")
	oldGap, oldTimeout := gapOf, infraGapTimeout
	t.Cleanup(func() { gapOf, infraGapTimeout = oldGap, oldTimeout })
	infraGapTimeout = 50 * time.Millisecond
	gapOf = func(ctx context.Context) (infra.Report, error) {
		<-ctx.Done()
		return infra.Report{}, ctx.Err()
	}

	start := time.Now()
	h := &nodeHost{log: quietLog()}
	if r := h.infraReport(context.Background()); r != nil {
		t.Errorf("a gap that was never answered is a report: %+v", r)
	}
	if r, err := infraStatus(context.Background(), nil); r != nil || err == nil {
		t.Errorf("status: %+v, %v", r, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the questions took %s", d)
	}
}
