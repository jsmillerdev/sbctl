package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStore(t *testing.T) { testStore(t, NewMemoryStore(), "abcdefghijklmnopqrst") }

// testStore is the conformance suite of the Store interface. ref must name a project
// the store accepts (the Postgres store has foreign keys to the registry).
func testStore(t *testing.T, s Store, ref string) {
	t.Helper()
	ctx := context.Background()

	// users
	u, err := s.UpsertUser(ctx, User{UserID: "aaaaaaaa-2222-4333-8444-555555555555", Email: "a@example.test", Username: "a"})
	if err != nil || u.ID == 0 || u.Email != "a@example.test" {
		t.Fatalf("upsert user: %+v %v", u, err)
	}
	u.FirstName = "Ada"
	if err := s.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	again, _ := s.UpsertUser(ctx, User{UserID: u.UserID, Email: "other@example.test", Username: "other"})
	if again.ID != u.ID || again.FirstName != "Ada" || again.Email != "a@example.test" || again.Username != "a" {
		t.Fatalf("upsert must keep stored profile: %+v", again)
	}
	if got, err := s.GetUserByID(ctx, u.ID); err != nil || got.UserID != u.UserID {
		t.Fatalf("by id: %+v %v", got, err)
	}
	if _, err := s.GetUser(ctx, "99999999-2222-4333-8444-555555555555"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	if list, err := s.ListUsers(ctx); err != nil || len(list) < 1 {
		t.Fatalf("list users: %v %v", list, err)
	}

	// login sessions: taken once, expired ones are gone
	sess := LoginSession{SessionID: "9d3b3d2a-8a51-4f6e-8c7e-0b2f4f6a1a11", UserID: u.UserID, ServerPublicKey: "04ab", Nonce: "0123456789abcdef01234567", Ciphertext: "ff", ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.PutLoginSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeLoginSession(ctx, sess.SessionID)
	if err != nil || got.Nonce != sess.Nonce || got.Ciphertext != "ff" {
		t.Fatalf("take: %+v %v", got, err)
	}
	if _, err := s.TakeLoginSession(ctx, sess.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second take: %v", err)
	}
	// wrong codes are counted in place; Put resets the count
	_ = s.PutLoginSession(ctx, sess)
	for want := 1; want <= 2; want++ {
		if n, err := s.FailLoginSession(ctx, sess.SessionID); err != nil || n != want {
			t.Fatalf("fail %d: %d %v", want, n, err)
		}
	}
	if g, err := s.GetLoginSession(ctx, sess.SessionID); err != nil || g.Failures != 2 {
		t.Fatalf("get after failures: %+v %v", g, err)
	}
	_ = s.PutLoginSession(ctx, sess)
	if g, _ := s.GetLoginSession(ctx, sess.SessionID); g == nil || g.Failures != 0 {
		t.Fatalf("put must reset failures: %+v", g)
	}
	if _, err := s.FailLoginSession(ctx, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fail of a missing session: %v", err)
	}
	// expired sessions are still returned (so their token can be deleted), and reaped once
	sess.ExpiresAt = time.Now().Add(-time.Second)
	_ = s.PutLoginSession(ctx, sess)
	if g, err := s.GetLoginSession(ctx, sess.SessionID); err != nil || g.ExpiresAt.After(time.Now()) {
		t.Fatalf("expired session: %+v %v", g, err)
	}
	if reaped, err := s.ReapLoginSessions(ctx); err != nil || len(reaped) != 1 || reaped[0].SessionID != sess.SessionID {
		t.Fatalf("reap: %+v %v", reaped, err)
	}
	if reaped, _ := s.ReapLoginSessions(ctx); len(reaped) != 0 {
		t.Fatalf("second reap: %+v", reaped)
	}
	if _, err := s.TakeLoginSession(ctx, sess.SessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("take after reap: %v", err)
	}

	// functions
	f := &Function{Ref: ref, Slug: "hello", Name: "Hello", Status: "ACTIVE", VerifyJWT: true, EntrypointPath: "index.ts"}
	if err := s.UpsertFunction(ctx, f, []FunctionFile{{Path: "index.ts", Content: []byte("v1")}, {Path: "lib/a.ts", Content: []byte{0, 1, 2}}}); err != nil || f.Version != 1 || f.ID == "" {
		t.Fatalf("upsert fn: %+v %v", f, err)
	}
	f2 := &Function{Ref: ref, Slug: "hello", Name: "Hello", Status: "ACTIVE", VerifyJWT: false, EntrypointPath: "index.ts"}
	if err := s.UpsertFunction(ctx, f2, []FunctionFile{{Path: "index.ts", Content: []byte("v2")}}); err != nil || f2.Version != 2 || f2.ID != f.ID {
		t.Fatalf("redeploy: %+v %v", f2, err)
	}
	files, err := s.FunctionFiles(ctx, ref, "hello")
	if err != nil || len(files) != 1 || string(files[0].Content) != "v2" {
		t.Fatalf("files replaced on redeploy: %+v %v", files, err)
	}
	// metadata-only update keeps files
	f2.Name = "Renamed"
	if err := s.UpsertFunction(ctx, f2, nil); err != nil || f2.Version != 3 {
		t.Fatalf("metadata update: %+v %v", f2, err)
	}
	if files, _ := s.FunctionFiles(ctx, ref, "hello"); len(files) != 1 {
		t.Fatalf("files after metadata update: %+v", files)
	}
	if list, err := s.ListFunctions(ctx, ref); err != nil || len(list) != 1 || list[0].Name != "Renamed" || list[0].VerifyJWT {
		t.Fatalf("list fns: %+v %v", list, err)
	}
	if err := s.DeleteFunction(ctx, ref, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFunction(ctx, ref, "hello"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted fn: %v", err)
	}
	if files, _ := s.FunctionFiles(ctx, ref, "hello"); len(files) != 0 {
		t.Fatalf("files outlive their function: %+v", files)
	}

	// function secrets
	if err := s.PutFunctionSecrets(ctx, ref, map[string][]byte{"A": []byte("1"), "B": []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFunctionSecrets(ctx, ref, map[string][]byte{"A": []byte("3")}); err != nil {
		t.Fatal(err)
	}
	secs, err := s.ListFunctionSecrets(ctx, ref)
	if err != nil || len(secs) != 2 || secs[0].Name != "A" || string(secs[0].Sealed) != "3" {
		t.Fatalf("secrets: %+v %v", secs, err)
	}
	if err := s.DeleteFunctionSecrets(ctx, ref, []string{"A", "missing"}); err != nil {
		t.Fatal(err)
	}
	if secs, _ := s.ListFunctionSecrets(ctx, ref); len(secs) != 1 || secs[0].Name != "B" {
		t.Fatalf("after delete: %+v", secs)
	}

	// content and folders
	folder := &ContentFolder{Ref: ref, OwnerID: u.ID, Name: "reports"}
	if err := s.CreateFolder(ctx, folder); err != nil || folder.ID == "" {
		t.Fatalf("folder: %+v %v", folder, err)
	}
	if got, err := s.GetFolder(ctx, ref, folder.ID); err != nil || got.Name != "reports" {
		t.Fatalf("get folder: %+v %v", got, err)
	}
	if _, err := s.GetFolder(ctx, "otherref", folder.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("folder of another project: %v", err)
	}
	sub := &ContentFolder{Ref: ref, OwnerID: u.ID, Name: "sub", ParentID: &folder.ID}
	if err := s.CreateFolder(ctx, sub); err != nil {
		t.Fatal(err)
	}
	mk := func(name, typ, vis string, fav bool, folderID *string) *Content {
		c := &Content{Ref: ref, OwnerID: u.ID, Type: typ, Name: name, Visibility: vis, Favorite: fav, FolderID: folderID, Body: []byte(`{"sql":"select 1"}`)}
		if err := s.UpsertContent(ctx, c); err != nil || c.ID == "" {
			t.Fatalf("upsert content %s: %+v %v", name, c, err)
		}
		return c
	}
	a := mk("alpha", "sql", "user", true, nil)
	mk("beta", "sql", "project", false, &folder.ID)
	mk("gamma", "report", "user", false, nil)

	if all, _ := s.ListContent(ctx, ref, ContentQuery{}); len(all) != 3 {
		t.Fatalf("all content: %d", len(all))
	}
	if r, _ := s.ListContent(ctx, ref, ContentQuery{Type: "sql"}); len(r) != 2 {
		t.Fatalf("type filter: %d", len(r))
	}
	if r, _ := s.ListContent(ctx, ref, ContentQuery{Favorite: true}); len(r) != 1 || r[0].Name != "alpha" {
		t.Fatalf("favorites: %+v", r)
	}
	if r, _ := s.ListContent(ctx, ref, ContentQuery{Name: "ALP"}); len(r) != 1 {
		t.Fatalf("name filter: %+v", r)
	}
	if r, _ := s.ListContent(ctx, ref, ContentQuery{RootOnly: true}); len(r) != 2 {
		t.Fatalf("root only: %d", len(r))
	}
	if r, _ := s.ListContent(ctx, ref, ContentQuery{FolderID: &folder.ID}); len(r) != 1 || r[0].Name != "beta" {
		t.Fatalf("in folder: %+v", r)
	}
	if n, err := s.CountContent(ctx, ref, u.ID); err != nil || n.Private != 2 || n.Shared != 1 || n.Favorites != 1 {
		t.Fatalf("count: %+v %v", n, err)
	}
	a.Name, a.Description = "alpha2", "d"
	if err := s.UpsertContent(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetContent(ctx, ref, a.ID); err != nil || got.Name != "alpha2" || got.Description != "d" || string(got.Body) == "" {
		t.Fatalf("update content: %+v %v", got, err)
	}
	if _, err := s.GetContent(ctx, ref, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing content: %v", err)
	}
	if fs, _ := s.ListFolders(ctx, ref, nil); len(fs) != 1 || fs[0].Name != "reports" {
		t.Fatalf("root folders: %+v", fs)
	}
	if fs, _ := s.ListFolders(ctx, ref, &folder.ID); len(fs) != 1 || fs[0].Name != "sub" {
		t.Fatalf("child folders: %+v", fs)
	}
	if err := s.RenameFolder(ctx, ref, folder.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameFolder(ctx, ref, "00000000-0000-4000-8000-000000000000", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename missing folder: %v", err)
	}
	// Deleting a folder removes sub-folders and keeps its content, now at the root.
	if err := s.DeleteFolders(ctx, ref, []string{folder.ID}); err != nil {
		t.Fatal(err)
	}
	if fs, _ := s.ListFolders(ctx, ref, nil); len(fs) != 0 {
		t.Fatalf("folders after delete: %+v", fs)
	}
	if r, _ := s.ListContent(ctx, ref, ContentQuery{RootOnly: true}); len(r) != 3 {
		t.Fatalf("content must survive its folder: %d", len(r))
	}
	priv := mk("private", "sql", "user", false, nil)
	if got, err := s.DeleteContent(ctx, ref, u.ID+1000, []string{priv.ID}); err != nil || len(got) != 0 {
		t.Fatalf("another user's private item was deleted: %v %v", got, err)
	}
	if got, err := s.DeleteContent(ctx, ref, u.ID, []string{priv.ID}); err != nil || len(got) != 1 {
		t.Fatalf("owner could not delete: %v %v", got, err)
	}
	if _, err := s.GetFolder(ctx, ref, "00000000-0000-4000-8000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing folder: %v", err)
	}
	deleted, err := s.DeleteContent(ctx, ref, u.ID, []string{a.ID, "00000000-0000-4000-8000-000000000000"})
	if err != nil || len(deleted) != 1 || deleted[0] != a.ID {
		t.Fatalf("delete content: %v %v", deleted, err)
	}
}
