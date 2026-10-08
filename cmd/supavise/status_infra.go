package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/infra"
)

// infraGapTimeout bounds the question to the cloud. A slow metadata service or EC2 API on a
// machine that only looks like EC2 must not hold up `supavise status`, an upgrade's plan or an
// emergency `supavise rollback`; an answer that does not come in time is no answer.
var infraGapTimeout = 8 * time.Second

// gapOf is infra.Gap; a test replaces it.
var gapOf = infra.Gap

// infraGap asks for the gap for at most infraGapTimeout.
func infraGap(ctx context.Context) (infra.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, infraGapTimeout)
	defer cancel()
	return gapOf(ctx)
}

// infraStatus is the infrastructure block of `supavise status`; nil means there is none to show
// (a node that is not on a stack, or whose stack is up to date). The cloud is asked only on a
// machine that looks like EC2: elsewhere the question costs seconds and has no answer.
func infraStatus(ctx context.Context, cfg *config.Config) (*infra.Report, error) {
	if !ec2Likely() {
		return nil, nil
	}
	r, err := infraGap(ctx)
	if err != nil {
		return nil, err
	}
	if !r.Behind() {
		return nil, nil
	}
	return &r, nil
}

// hostBlock is the host block of `supavise status`: the steps of `supavise system converge` that
// have something to do. nil means the host is converged.
type hostBlock struct {
	Pending []hostsetup.Result `json:"pending"`
}

// hostStatusBlock is hostStatus as a block for the status report: nil when no step is pending.
func hostStatusBlock(ctx context.Context, cfg *config.Config) (*hostBlock, error) {
	pending, err := hostStatus(ctx, cfg)
	if err != nil || len(pending) == 0 {
		return nil, err
	}
	return &hostBlock{Pending: pending}, nil
}

// Render writes the block: what is pending and the command that does it.
func (b *hostBlock) Render(w io.Writer) {
	fmt.Fprintf(w, "Host  %d step(s) of the host layer are pending\n", len(b.Pending))
	for _, r := range b.Pending {
		line := "  pending  " + r.Title
		if r.Detail != "" {
			line += " (" + r.Detail + ")"
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w, "  Fix: sudo supavise system converge")
}

// hostStatus lists the steps of the host layer that have something to do, from the same checks as
// `supavise system converge --check`; empty when the host is converged. It needs no root.
func hostStatus(ctx context.Context, cfg *config.Config) ([]hostsetup.Result, error) {
	c, err := newConverger(cfg, defaultUnitDir, "/etc/polkit-1/rules.d", false, io.Discard, io.Discard)
	if err != nil {
		return nil, err
	}
	var pending []hostsetup.Result
	for _, r := range c.Check(ctx) {
		if r.Pending {
			pending = append(pending, r)
		}
	}
	return pending, nil
}
