package replicas

import (
	"context"
	"strings"
	"testing"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/lifecycle"
	"github.com/supavise/supavise/internal/registry"
)

func TestRoomAdmitter(t *testing.T) {
	const gib = int64(1) << 30
	cfg := config.Default()
	cfg.Compute.Overcommit = 2 // a budget of twice the node's memory
	project := func(ref, class string) registry.Project {
		return registry.Project{Ref: ref, Class: class, Status: registry.StatusActiveHealthy}
	}
	node := registry.Node{ID: "n2", Name: "eu"}
	tests := []struct {
		name   string
		room   Room
		hosted []registry.Project
		p      registry.Project
		seed   int64
		want   string // "" fits, else a part of the refusal
	}{
		{"empty node", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}}, nil, project(refA, "small"), 0, ""},
		{"unknown resources fit everything", Room{}, nil, project(refA, "16xlarge"), 0, ""},
		{"the last 2 GB of a 16 GB budget", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}},
			[]registry.Project{project("h1", "large"), project("h2", "medium"), project("h3", "small")}, project(refA, "small"), 0, ""},
		{"nothing left of the budget", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}},
			[]registry.Project{project("h1", "large"), project("h2", "medium"), project("h3", "small"), project("h4", "small")}, project(refA, "small"), 0, "cannot run a Small project"},
		{"a project that is paused holds no room", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}},
			[]registry.Project{project("h1", "large"), {Ref: "h2", Class: "large", Status: registry.StatusInactive}}, project(refA, "large"), 0, ""},
		{"a hand-set cap counts", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}},
			[]registry.Project{project("h1", "large"), project("h2", "medium"), project("h3", "small")},
			registry.Project{Ref: refA, Class: "small", Status: registry.StatusActiveHealthy, Limits: config.Limits{MemoryMax: "4G"}}, 0, "cannot run a Small project"},
		{"more cores than the node has", Room{Resources: lifecycle.NodeResources{MemoryBytes: 64 * gib, CPUs: 2}}, nil, project(refA, "xlarge"), 0, "cores"},
		{"disk for the seed and headroom", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}, DiskKnown: true, FreeDiskBytes: 10 * gib}, nil, project(refA, "small"), 6 * gib, ""},
		{"not enough disk", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}, DiskKnown: true, FreeDiskBytes: 10 * gib}, nil, project(refA, "small"), 8 * gib, "10.0 GB of free disk and a replica of " + refA + " needs about 11.0 GB"},
		{"disk unknown is not checked", Room{Resources: lifecycle.NodeResources{MemoryBytes: 8 * gib}}, nil, project(refA, "small"), 100 * gib, ""},
		{"a size that does not exist", Room{}, nil, project(refA, "gigantic"), 0, "unknown project size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := RoomAdmitter{Cfg: cfg, Room: func(context.Context, registry.Node) Room { return tt.room }}
			err := a.Admit(context.Background(), AdmitRequest{Node: node, Project: tt.p, Hosted: tt.hosted, SeedBytes: tt.seed})
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("got %v, want a refusal with %q", err, tt.want)
			}
		})
	}
}

func TestDiskNeed(t *testing.T) {
	if got := diskNeed(4 << 30); got != 4<<30+1<<30+1<<30 {
		t.Fatalf("diskNeed(4 GiB) = %d", got)
	}
	if got := diskNeed(0); got != diskHeadroom {
		t.Fatalf("diskNeed(0) = %d", got)
	}
}
