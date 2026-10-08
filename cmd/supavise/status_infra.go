package main

import (
	"context"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/infra"
)

// infraStatus is the infrastructure block of `supavise status`; nil means there is none to show
// (a node that is not on a stack, or whose stack is up to date).
func infraStatus(ctx context.Context, cfg *config.Config) (*infra.Report, error) {
	return nil, nil
}
