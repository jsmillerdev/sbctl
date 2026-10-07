package main

import (
	"strings"
	"testing"

	"github.com/OWNER/sbctl/internal/branching"
	"github.com/OWNER/sbctl/internal/registry"
)

func TestBranchesCommandTree(t *testing.T) {
	var branches []string
	for _, c := range rootCmd.Commands() {
		if c.Name() == "branches" {
			for _, s := range c.Commands() {
				branches = append(branches, s.Name())
			}
		}
	}
	for _, want := range []string{"list", "create", "get", "delete", "merge", "reset", "push", "sweep", "diff", "restore", "seed"} {
		found := false
		for _, b := range branches {
			found = found || b == want
		}
		if !found {
			t.Errorf("sbctl branches %s is missing (have %v)", want, branches)
		}
	}
}

func TestPrintBranch(t *testing.T) {
	var sb strings.Builder
	b := &branching.Branch{ID: "i", Name: "feat", Ref: "r", ParentRef: "p", State: registry.BranchMigrationsPassed, ProjectStatus: registry.StatusActiveHealthy, CloneMethod: "clonefile", Detail: "ok"}
	printBranch(&sb, b)
	for _, want := range []string{"name:     feat", "status:   MIGRATIONS_PASSED (project ACTIVE_HEALTHY)", "data:     clonefile", "detail:   ok"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("missing %q in\n%s", want, sb.String())
		}
	}
}
