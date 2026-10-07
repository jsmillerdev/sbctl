package projectconfig

import (
	"fmt"
	"strconv"
	"strings"
)

// Postgres refuses to start when a setting is out of its range, and most of these take
// effect only at start, so a bad value saved today would keep the project from coming back
// after the next restart. Every size and duration is therefore parsed here with Postgres'
// own units and checked against the documented range of its setting, and a bare number
// (whose unit depends on the setting) is accepted only as 0 or -1.

var sizeUnits = map[string]float64{"B": 1, "kB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}
var timeUnits = map[string]float64{"us": 0.001, "ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000}

func splitUnit(s string) (num float64, unit string, err error) {
	s = strings.TrimSpace(s)
	i := len(s)
	for i > 0 && (s[i-1] < '0' || s[i-1] > '9') {
		i--
	}
	if i == 0 {
		return 0, "", fmt.Errorf("%q is not a number with a unit", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", err
	}
	return n, strings.TrimSpace(s[i:]), nil
}

// ParseSize returns the bytes of a Postgres size ("128MB", "8 kB"); -1 and 0 stand for
// themselves. A number without a unit is an error.
func ParseSize(s string) (float64, error) {
	n, unit, err := splitUnit(s)
	if err != nil {
		return 0, err
	}
	if unit == "" {
		if n == 0 || n == -1 {
			return n, nil
		}
		return 0, fmt.Errorf("%q needs a unit (B, kB, MB, GB or TB)", s)
	}
	u, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit %q (B, kB, MB, GB or TB)", s, unit)
	}
	return n * u, nil
}

// ParseDuration returns the milliseconds of a Postgres duration ("30s", "5min").
func ParseDuration(s string) (float64, error) {
	n, unit, err := splitUnit(s)
	if err != nil {
		return 0, err
	}
	if unit == "" {
		if n == 0 || n == -1 {
			return n, nil
		}
		return 0, fmt.Errorf("%q needs a unit (us, ms, s, min, h or d)", s)
	}
	u, ok := timeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown unit %q (us, ms, s, min, h or d)", s, unit)
	}
	return n * u, nil
}

func sizeRange(min, max float64, allowMinusOne bool) func(any) error {
	return func(v any) error {
		b, err := ParseSize(v.(string))
		if err != nil {
			return err
		}
		if b == -1 && allowMinusOne {
			return nil
		}
		if b < min || b > max {
			return fmt.Errorf("must be between %s and %s", humanBytes(min), humanBytes(max))
		}
		return nil
	}
}

func durRange(minMs, maxMs float64, allowMinusOne bool) func(any) error {
	return func(v any) error {
		ms, err := ParseDuration(v.(string))
		if err != nil {
			return err
		}
		if ms == -1 && allowMinusOne {
			return nil
		}
		if ms < minMs || ms > maxMs {
			return fmt.Errorf("must be between %s and %s", humanMs(minMs), humanMs(maxMs))
		}
		return nil
	}
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<30 && int64(b)%(1<<30) == 0:
		return fmt.Sprintf("%dGB", int64(b)>>30)
	case b >= 1<<20 && int64(b)%(1<<20) == 0:
		return fmt.Sprintf("%dMB", int64(b)>>20)
	case b >= 1<<10 && int64(b)%(1<<10) == 0:
		return fmt.Sprintf("%dkB", int64(b)>>10)
	}
	return fmt.Sprintf("%dB", int64(b))
}

func humanMs(ms float64) string {
	switch {
	case ms >= 86400000 && int64(ms)%86400000 == 0:
		return fmt.Sprintf("%dd", int64(ms)/86400000)
	case ms >= 1000 && int64(ms)%1000 == 0:
		return fmt.Sprintf("%ds", int64(ms)/1000)
	}
	return fmt.Sprintf("%dms", int64(ms))
}

const (
	kB     = 1 << 10
	mB     = 1 << 20
	gB     = 1 << 30
	intKB  = 2147483647 * kB // INT_MAX kilobytes, the ceiling of the *_mem settings
	intMs  = 2147483647.0
	second = 1000.0
)

// pgRanges are the documented ranges (PostgreSQL 17, "Server Configuration") of the
// settings whose range is stricter than the type.
func init() {
	for i := range PostgresSchema.Fields {
		f := &PostgresSchema.Fields[i]
		switch f.Name {
		case "shared_buffers":
			f.Check = sizeRange(128*kB, 1<<40, false)
		case "effective_cache_size":
			f.Check = sizeRange(8*kB, 1<<50, false)
		case "work_mem":
			f.Check = sizeRange(64*kB, intKB, false)
		case "maintenance_work_mem":
			f.Check = sizeRange(1*mB, intKB, false)
		case "logical_decoding_work_mem":
			f.Check = sizeRange(64*kB, intKB, false)
		case "max_wal_size":
			f.Check = sizeRange(32*mB, 1<<50, false)
		case "track_activity_query_size":
			f.Check = sizeRange(100, 1*mB, false)
		case "max_slot_wal_keep_size", "wal_keep_size":
			f.Check = sizeRange(0, 1<<50, f.Name == "max_slot_wal_keep_size")
		case "log_temp_files":
			f.Check = sizeRange(0, intKB, true)
		case "log_autovacuum_min_duration", "max_standby_archive_delay", "max_standby_streaming_delay":
			f.Check = durRange(0, intMs, true)
		case "log_startup_progress_interval", "wal_sender_timeout", "statement_timeout":
			f.Check = durRange(0, intMs, false)
		case "checkpoint_timeout":
			f.Check = durRange(30*second, 86400000, false)
		}
		if f.Kind == String && f.Check != nil {
			f.Normalize = func(v any) any { return strings.ReplaceAll(v.(string), " ", "") }
		}
	}
	PostgresSchema.Cross = postgresCross
}

// postgresCross rejects combinations that would starve the project's own services or
// exceed what its memory limit allows. memory limit is only known to the caller.
func postgresCross(eff, _ Values, cx CrossContext) error {
	size := func(name string) (float64, bool) {
		s := eff.Str(name)
		if s == "" {
			return 0, false
		}
		b, err := ParseSize(s)
		return b, err == nil
	}
	if n, ok := eff.Int("max_connections"); ok && n < 20 {
		// GoTrue, PostgREST, Realtime, Storage, Supavisor's pool and pg-meta hold about this many.
		return invalid("max_connections must be at least 20: the project's own services keep connections open")
	}
	if n, ok := eff.Int("max_wal_senders"); ok && n < 3 {
		return invalid("max_wal_senders must be at least 3: base backups, archiving and Realtime's replication use them")
	}
	if n, ok := eff.Int("max_replication_slots"); ok && n < 2 {
		return invalid("max_replication_slots must be at least 2: Realtime and base backups use slots")
	}
	if n, ok := eff.Int("max_worker_processes"); ok && n < 4 {
		return invalid("max_worker_processes must be at least 4")
	}
	if mc, ok := eff.Int("max_connections"); ok {
		if n, ok := eff.Int("max_wal_senders"); ok && n > mc {
			return invalid("max_wal_senders (%d) cannot exceed max_connections (%d)", n, mc)
		}
	}
	if cx.MemoryLimit > 0 {
		lim := float64(cx.MemoryLimit)
		if b, ok := size("shared_buffers"); ok && b > lim*0.4 {
			return invalid("shared_buffers %s is more than 40%% of the project's memory limit (%s)", eff.Str("shared_buffers"), humanBytes(lim))
		}
		for _, n := range []string{"work_mem", "maintenance_work_mem", "logical_decoding_work_mem"} {
			if b, ok := size(n); ok && b > lim*0.25 {
				return invalid("%s %s is more than 25%% of the project's memory limit (%s)", n, eff.Str(n), humanBytes(lim))
			}
		}
		if n, ok := eff.Int("max_connections"); ok && float64(n)*4*mB > lim && n > 100 {
			return invalid("max_connections %d is too many for the project's memory limit (%s): each connection needs a few MB", n, humanBytes(lim))
		}
	} else if n, ok := eff.Int("max_connections"); ok && n > 1000 {
		return invalid("max_connections above 1000 needs a memory limit on the project")
	}
	return postmasterMemory(eff, cx)
}

// Per-entry and per-backend shared memory the postmaster allocates at start, rounded up:
// a lock table entry (LOCK and PROCLOCK with their hash overhead) and a backend's share of
// the process array, semaphores and fixed-size queues.
const (
	lockEntryBytes   = 300
	backendSlotBytes = 128 * kB
	// With no memory limit to compare against, what the postmaster allocates for locks and
	// backend slots stays under what any node running a project can spare.
	unlimitedLockBytes = 64 * mB
	unlimitedBackends  = 600
)

// postmasterMemory rejects a combination of postmaster-context settings whose shared memory
// the project cannot allocate. Postgres accepts such a value in ALTER SYSTEM and fails to
// start with it ("out of memory" while creating shared memory), which leaves the project's
// database down at its next restart, and a paused or rebooted project would not come back.
func postmasterMemory(eff Values, cx CrossContext) error {
	intOr := func(name string, def int64) int64 {
		if n, ok := eff.Int(name); ok {
			return n
		}
		return def
	}
	// Postgres' defaults stand in for settings that were not saved (the class's sizing is
	// not known here and is smaller).
	conns := intOr("max_connections", 100)
	workers := intOr("max_worker_processes", 8)
	senders := intOr("max_wal_senders", 10)
	backends := conns + workers + senders + 3 /* autovacuum workers */ + 1
	if backends > 0x3FFFF {
		return invalid("max_connections + max_worker_processes + max_wal_senders must stay below %d", 0x3FFFF)
	}
	locks := intOr("max_locks_per_transaction", 64)
	lockBytes := float64(locks) * float64(backends) * lockEntryBytes
	slotBytes := float64(backends) * backendSlotBytes
	shared := lockBytes + slotBytes
	if b, err := ParseSize(eff.Str("shared_buffers")); err == nil && b > 0 {
		shared += b
	}
	if cx.MemoryLimit > 0 {
		lim := float64(cx.MemoryLimit)
		if lockBytes > lim*0.10 {
			return invalid("max_locks_per_transaction %d with %d backends needs about %s of shared memory for the lock table, more than 10%% of the project's memory limit (%s)", locks, backends, humanBytes(lockBytes), humanBytes(lim))
		}
		if shared > lim*0.60 {
			return invalid("the shared memory these settings ask for (about %s) is more than 60%% of the project's memory limit (%s)", humanBytes(shared), humanBytes(lim))
		}
		return nil
	}
	if lockBytes > unlimitedLockBytes || backends > unlimitedBackends {
		return invalid("max_locks_per_transaction %d with %d backends (max_connections, max_worker_processes and max_wal_senders together) needs a memory limit on the project", locks, backends)
	}
	return nil
}
