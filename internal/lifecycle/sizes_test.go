package lifecycle

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/config"
	"github.com/jsmillerdev/supavise/internal/projectconfig"
	"github.com/jsmillerdev/supavise/internal/secrets"
)

// hostedRow is a row of the compute table at supabase.com/docs/guides/platform/compute-and-disk:
// memory (GB), vCPUs (0: shared), max connections, replication slots and WAL senders, pooler clients.
type hostedRow struct {
	name, variant, title        string
	memGB                       float64
	conns, slots, clients, vcpu int
}

var hostedTable = []hostedRow{
	{"nano", "ci_nano", "Nano", 0.5, 60, 5, 200, 1},
	{"micro", "ci_micro", "Micro", 1, 60, 5, 200, 1},
	{"small", "ci_small", "Small", 2, 90, 5, 400, 1},
	{"medium", "ci_medium", "Medium", 4, 120, 5, 600, 2},
	{"large", "ci_large", "Large", 8, 160, 8, 800, 2},
	{"xlarge", "ci_xlarge", "XL", 16, 240, 24, 1000, 4},
	{"2xlarge", "ci_2xlarge", "2XL", 32, 380, 80, 1500, 8},
	{"4xlarge", "ci_4xlarge", "4XL", 64, 480, 80, 3000, 16},
	{"8xlarge", "ci_8xlarge", "8XL", 128, 490, 80, 6000, 32},
	{"12xlarge", "ci_12xlarge", "12XL", 192, 500, 80, 9000, 48},
	{"16xlarge", "ci_16xlarge", "16XL", 256, 500, 80, 12000, 64},
}

// The published columns of the table are hosted's, row by row.
func TestSizeTableMatchesHosted(t *testing.T) {
	if got := ClassNames(); len(got) != len(hostedTable) {
		t.Fatalf("sizes = %v", got)
	}
	for i, h := range hostedTable {
		c := Classes()[i]
		if c.Name != h.name || c.Variant != h.variant || c.Title != h.title {
			t.Errorf("row %d names = %s %s %s", i, c.Name, c.Variant, c.Title)
		}
		if c.MemoryGB() != h.memGB || c.MaxConnections != h.conns || c.MaxReplicationSlots != h.slots || c.MaxWALSenders != h.slots || c.PoolerMaxClients != h.clients {
			t.Errorf("%s: %+v", h.name, c)
		}
		if c.VCPUs() != h.vcpu {
			t.Errorf("%s: vcpus = %d, want %d", h.name, c.VCPUs(), h.vcpu)
		}
		if c.Shared != (i <= 3) {
			t.Errorf("%s: shared CPU = %v (hosted: up to Medium)", h.name, c.Shared)
		}
		if got, ok := ClassByVariant(h.variant); !ok || got.Name != h.name {
			t.Errorf("variant %s resolves to %v", h.variant, got.Name)
		}
	}
}

// The derived columns follow the rules at newSize; this pins what they give.
func TestSizeTableDerivedSettings(t *testing.T) {
	type want struct {
		sb, ecs, wm, mwm, wal string
		workers, pool          int
		mem, cpu               string
	}
	for name, w := range map[string]want{
		"nano":     {"128MB", "384MB", "4MB", "32MB", "128MB", 16, 20, "512M", "100%"},
		"micro":    {"256MB", "768MB", "4MB", "64MB", "256MB", 16, 20, "1G", "100%"},
		"small":    {"512MB", "1536MB", "5MB", "128MB", "512MB", 16, 35, "2G", "100%"},
		"medium":   {"1GB", "3GB", "8MB", "256MB", "1GB", 16, 45, "4G", "200%"},
		"large":    {"2GB", "6GB", "12MB", "512MB", "2GB", 16, 60, "8G", "200%"},
		"xlarge":   {"4GB", "12GB", "17MB", "1GB", "4GB", 16, 95, "16G", "400%"},
		"2xlarge":  {"8GB", "24GB", "21MB", "2GB", "8GB", 24, 150, "32G", "800%"},
		"4xlarge":  {"16GB", "48GB", "34MB", "2GB", "8GB", 40, 190, "64G", "1600%"},
		"8xlarge":  {"32GB", "96GB", "64MB", "2GB", "8GB", 72, 195, "128G", "3200%"},
		"16xlarge": {"64GB", "192GB", "64MB", "2GB", "8GB", 136, 200, "256G", "6400%"},
	} {
		c, err := ClassFor(name)
		if err != nil {
			t.Fatal(err)
		}
		if c.SharedBuffers != w.sb || c.EffectiveCacheSize != w.ecs || c.WorkMem != w.wm || c.MaintenanceWorkMem != w.mwm || c.MaxWALSize != w.wal || c.MaxWorkerProcesses != w.workers || c.PoolSize != w.pool {
			t.Errorf("%s: %+v", name, c)
		}
		if l := c.Limits(); l.MemoryMax != w.mem || l.CPUQuota != w.cpu {
			t.Errorf("%s: limits = %+v", name, l)
		}
	}
}

// Every size keeps pg_cron's background-worker floor and stays inside the rules that validate a
// saved Postgres setting, so a client that saves back what the dashboard showed is not refused.
func TestSizesKeepCronFloorAndPassSettingsValidation(t *testing.T) {
	sec, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m := projectconfig.NewManager(projectconfig.NewMemory(), sec, projectconfig.Options{})
	for _, c := range Classes() {
		if c.MaxWorkerProcesses < MinWorkerProcesses {
			t.Errorf("%s: max_worker_processes = %d, below the pg_cron floor %d", c.Name, c.MaxWorkerProcesses, MinWorkerProcesses)
		}
		got := strings.Join(c.Settings(), " ")
		if !strings.Contains(got, "max_worker_processes="+strconv.Itoa(c.MaxWorkerProcesses)) {
			t.Errorf("%s: settings lack the worker count: %s", c.Name, got)
		}
		if c.MaxWALSenders > c.MaxConnections {
			t.Errorf("%s: more WAL senders than connections", c.Name)
		}
		patch := map[string]any{}
		for _, s := range c.Settings() {
			k, v, _ := strings.Cut(s, "=")
			if n, err := strconv.Atoi(v); err == nil {
				patch[k] = float64(n)
			} else {
				patch[k] = v
			}
		}
		if _, err := m.Patch(context.Background(), "ref"+c.Name, projectconfig.Postgres, patch, projectconfig.CrossContext{MemoryLimit: c.MemoryBytes}); err != nil {
			t.Errorf("%s: the size's own settings are refused: %v", c.Name, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]string{
		"micro": "micro", "Micro": "micro", " SMALL ": "small", "ci_medium": "medium", "xl": "xlarge", "XL": "xlarge",
		"xlarge": "xlarge", "2XL": "2xlarge", "2xlarge": "2xlarge", "ci_16xlarge": "16xlarge", "16xl": "16xlarge",
		"default": "micro", "pico": "nano", "ci_nano": "nano",
	} {
		if got, ok := ParseSize(in); !ok || got != want {
			t.Errorf("ParseSize(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "huge", "system", "3xl", "24xlarge", "ci_"} {
		if got, ok := ParseSize(in); ok {
			t.Errorf("ParseSize(%q) = %q, want unknown", in, got)
		}
	}
	if c, err := ClassFor(ClassSystem); err != nil || c.Name != ClassSystem {
		t.Errorf("the system profile: %+v %v", c, err)
	}
}

// A new project's limits are its size's; the defaults of config.toml size the shared services only.
func TestCreateGivesTheProjectItsSizeLimits(t *testing.T) {
	h := newHarness(t)
	h.cfg.Defaults = config.Limits{MemoryMax: "3G", CPUQuota: "300%"}
	p, err := h.e.Create(context.Background(), CreateRequest{Name: "a", Class: "Small"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Class != "small" || p.Limits != (config.Limits{MemoryMax: "2G", CPUQuota: "100%"}) {
		t.Fatalf("class %q limits %+v", p.Class, p.Limits)
	}
	q, err := h.e.Create(context.Background(), CreateRequest{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if q.Class != "micro" || q.Limits.MemoryMax != "1G" {
		t.Fatalf("default project: class %q limits %+v", q.Class, q.Limits)
	}
	// A legacy name from a backup manifest lands on a size.
	r, err := h.e.Create(context.Background(), CreateRequest{Name: "c", Class: "default"})
	if err != nil || r.Class != "micro" {
		t.Fatalf("legacy class: %v %v", r, err)
	}
}
