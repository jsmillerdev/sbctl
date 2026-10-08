package main

import (
	"context"
	"io"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/hostsetup"
	"github.com/supavise/supavise/internal/infra"
)

// infraStatus is the infrastructure block of `supavise status`; nil means there is none to show
// (a node that is not on a stack, or whose stack is up to date). The cloud is asked only on a
// machine that looks like EC2: elsewhere the question costs seconds and has no answer.
func infraStatus(ctx context.Context, cfg *config.Config) (*infra.Report, error) {
	if !ec2Likely() {
		return nil, nil
	}
	r, err := infra.Gap(ctx)
	if err != nil {
		return nil, err
	}
	if !r.Behind() {
		return nil, nil
	}
	return &r, nil
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
