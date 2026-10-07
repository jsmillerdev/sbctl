package functions

import (
	"context"
	"errors"

	"github.com/jsmillerdev/supavise/internal/api"
	"github.com/jsmillerdev/supavise/internal/backup"
)

// BackupStore adapts the API's store of deployments and secrets to what backups read and
// restores write (backup.Functions). The API imports the backup package, so the adapter
// lives here. After a restore the next reconcile (or the next SyncProject) puts the
// restored deployments where the runtime reads them.
func BackupStore(st api.Store) backup.Functions { return backupStore{st} }

type backupStore struct{ st api.Store }

func (b backupStore) ListFunctions(ctx context.Context, ref string) ([]backup.FunctionRecord, error) {
	fs, err := b.st.ListFunctions(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make([]backup.FunctionRecord, len(fs))
	for i, f := range fs {
		out[i] = backup.FunctionRecord{Slug: f.Slug, ID: f.ID, Name: f.Name, Version: f.Version, Status: f.Status,
			VerifyJWT: f.VerifyJWT, EntrypointPath: f.EntrypointPath, ImportMapPath: f.ImportMapPath,
			CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt}
	}
	return out, nil
}

func (b backupStore) FunctionFiles(ctx context.Context, ref, slug string) ([]backup.FunctionFile, error) {
	fl, err := b.st.FunctionFiles(ctx, ref, slug)
	if errors.Is(err, api.ErrNotFound) {
		return nil, backup.ErrNoFunction
	}
	if err != nil {
		return nil, err
	}
	out := make([]backup.FunctionFile, len(fl))
	for i, f := range fl {
		out[i] = backup.FunctionFile{Path: f.Path, Content: f.Content}
	}
	return out, nil
}

func (b backupStore) ListFunctionSecrets(ctx context.Context, ref string) ([]backup.FunctionSecret, error) {
	ss, err := b.st.ListFunctionSecrets(ctx, ref)
	if err != nil {
		return nil, err
	}
	out := make([]backup.FunctionSecret, len(ss))
	for i, s := range ss {
		out[i] = backup.FunctionSecret{Name: s.Name, Sealed: s.Sealed}
	}
	return out, nil
}

func (b backupStore) RestoreFunction(ctx context.Context, ref string, f backup.FunctionRecord, files []backup.FunctionFile) error {
	fl := make([]api.FunctionFile, len(files))
	for i, x := range files {
		fl[i] = api.FunctionFile{Path: x.Path, Content: x.Content}
	}
	return b.st.RestoreFunction(ctx, api.Function{Ref: ref, Slug: f.Slug, ID: f.ID, Name: f.Name, Version: f.Version, Status: f.Status,
		VerifyJWT: f.VerifyJWT, EntrypointPath: f.EntrypointPath, ImportMapPath: f.ImportMapPath,
		CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt}, fl)
}

func (b backupStore) DeleteFunction(ctx context.Context, ref, slug string) error {
	err := b.st.DeleteFunction(ctx, ref, slug)
	if errors.Is(err, api.ErrNotFound) {
		return backup.ErrNoFunction
	}
	return err
}

func (b backupStore) RestoreFunctionSecrets(ctx context.Context, ref string, sealed map[string][]byte) error {
	return b.st.PutFunctionSecrets(ctx, ref, sealed)
}

func (b backupStore) DeleteFunctionSecrets(ctx context.Context, ref string, names []string) error {
	return b.st.DeleteFunctionSecrets(ctx, ref, names)
}
