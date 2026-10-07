package registry

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpgradeStatus is the status of one upgrade, with the numbers of Studio's DatabaseUpgradeStatus
// (@supabase/shared-types), which the Management API reports unchanged.
type UpgradeStatus int

const (
	UpgradeRunning UpgradeStatus = 0
	UpgradeDone    UpgradeStatus = 1
	UpgradeFailed  UpgradeStatus = 2
)

// Upgrade is one upgrade of a project's service versions. Its progress and error values are
// the strings of the Management API's upgrade status (see lifecycle.UpgradeProgress).
type Upgrade struct {
	// TrackingID is the id POST /v1/projects/{ref}/upgrade returns.
	TrackingID string
	Ref        string
	// From and To map service name (config.Svc*) to artifact tag, as Project.Versions does.
	From, To map[string]string
	// TargetVersion is the postgres version the request named.
	TargetVersion  string
	Status         UpgradeStatus
	Progress       string
	Error          string // the API's error code of the stage that failed; empty unless Status is UpgradeFailed
	Detail         string // the cause, in words
	BackupID       int64  // the base backup taken before the upgrade; 0 when none was
	InitiatedAt    time.Time
	LatestStatusAt time.Time
}

// UpgradeStore keeps the upgrades of projects. Both registries implement it; the Engine and
// the Management API reach it through the Registry they hold.
type UpgradeStore interface {
	// PutUpgrade inserts or replaces the upgrade with u.TrackingID.
	PutUpgrade(ctx context.Context, u *Upgrade) error
	// LatestUpgrade returns the newest upgrade of ref, or ErrNotFound when it never had one.
	LatestUpgrade(ctx context.Context, ref string) (*Upgrade, error)
}

// Upgrades returns reg's UpgradeStore, or nil when it has none.
func Upgrades(reg Registry) UpgradeStore {
	s, _ := reg.(UpgradeStore)
	return s
}

var _ UpgradeStore = (*Memory)(nil)
var _ UpgradeStore = (*Postgres)(nil)

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (m *Memory) PutUpgrade(_ context.Context, u *Upgrade) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[u.Ref]; !ok {
		return ErrNotFound
	}
	c := *u
	c.From, c.To = cloneMap(u.From), cloneMap(u.To)
	for i := range m.upgrades {
		if m.upgrades[i].TrackingID == u.TrackingID {
			m.upgrades[i] = c
			return nil
		}
	}
	m.upgrades = append(m.upgrades, c)
	return nil
}

func (m *Memory) LatestUpgrade(_ context.Context, ref string) (*Upgrade, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []Upgrade
	for _, u := range m.upgrades {
		if u.Ref == ref {
			found = append(found, u)
		}
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].InitiatedAt.Before(found[j].InitiatedAt) })
	u := found[len(found)-1]
	u.From, u.To = cloneMap(u.From), cloneMap(u.To)
	return &u, nil
}

func (r *Postgres) PutUpgrade(ctx context.Context, u *Upgrade) error {
	from, _ := json.Marshal(nonNil(u.From))
	to, _ := json.Marshal(nonNil(u.To))
	_, err := r.pool.Exec(ctx, `
		insert into supavise.project_upgrades
		  (tracking_id, ref, from_versions, to_versions, target_version, status, progress, error, detail, backup_id, initiated_at, latest_status_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (tracking_id) do update set
		  from_versions = excluded.from_versions, to_versions = excluded.to_versions, target_version = excluded.target_version,
		  status = excluded.status, progress = excluded.progress, error = excluded.error, detail = excluded.detail,
		  backup_id = excluded.backup_id, latest_status_at = excluded.latest_status_at`,
		u.TrackingID, u.Ref, from, to, u.TargetVersion, int(u.Status), u.Progress, u.Error, u.Detail, u.BackupID, u.InitiatedAt, u.LatestStatusAt)
	return err
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func (r *Postgres) LatestUpgrade(ctx context.Context, ref string) (*Upgrade, error) {
	var u Upgrade
	var from, to []byte
	var status int
	err := r.pool.QueryRow(ctx, `
		select tracking_id::text, ref, from_versions, to_versions, target_version, status, progress, error, detail, backup_id, initiated_at, latest_status_at
		from supavise.project_upgrades where ref = $1 order by initiated_at desc limit 1`, ref).
		Scan(&u.TrackingID, &u.Ref, &from, &to, &u.TargetVersion, &status, &u.Progress, &u.Error, &u.Detail, &u.BackupID, &u.InitiatedAt, &u.LatestStatusAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.Status = UpgradeStatus(status)
	if err := json.Unmarshal(from, &u.From); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(to, &u.To); err != nil {
		return nil, err
	}
	return &u, nil
}
