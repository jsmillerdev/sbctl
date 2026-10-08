package storagemigrate

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func inv(s ...any) inventory {
	var out inventory
	for i := 0; i < len(s); i += 3 {
		out = append(out, fileState{Path: s[i].(string), Size: int64(s[i+1].(int)), MTime: int64(s[i+2].(int))})
	}
	return out
}

func TestDiffFindsNewChangedAndGoneFiles(t *testing.T) {
	prev := inv("a", 1, 1, "b", 2, 2, "c", 3, 3, "e", 5, 5)
	cur := inv("a", 1, 1, "b", 2, 9, "d", 4, 4, "e", 6, 5)
	changed, gone := diff(prev, cur)
	if want := []string{"b", "d", "e"}; !reflect.DeepEqual(paths(changed), want) {
		t.Errorf("changed %v, want %v", paths(changed), want)
	}
	if !reflect.DeepEqual(gone, []string{"c"}) {
		t.Errorf("gone %v", gone)
	}
	changed, gone = diff(nil, cur)
	if len(changed) != 4 || len(gone) != 0 {
		t.Errorf("against nothing: %v %v", changed, gone)
	}
	changed, gone = diff(cur, nil)
	if len(changed) != 0 || len(gone) != 4 {
		t.Errorf("against an empty directory: %v %v", changed, gone)
	}
}

func paths(f []fileState) []string {
	var out []string
	for _, x := range f {
		out = append(out, x.Path)
	}
	return out
}

func TestDiffBucketTrustsACopyThatIsNewerThanTheFile(t *testing.T) {
	at := time.Unix(1000, 0)
	cur := inv("a", 1, int(at.UnixNano()), "b", 2, int(at.UnixNano()), "c", 3, int(at.UnixNano()), "d", 4, int(at.UnixNano()))
	listed := []Entry{
		{Key: "p/a", Size: 1, ModTime: at.Add(time.Minute)},   // copied after the file was written
		{Key: "p/b", Size: 9, ModTime: at.Add(time.Minute)},   // other size
		{Key: "p/c", Size: 3, ModTime: at.Add(-time.Minute)},  // copied before the file was written
		{Key: "p/old", Size: 1, ModTime: at.Add(time.Minute)}, // no file any more
		{Key: "p/d", Size: 4, ModTime: at.Add(1)},             // too close to tell
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Key < listed[j].Key })
	changed := diffBucket(cur, listed, "p/")
	if want := []string{"b", "c", "d"}; !reflect.DeepEqual(paths(changed), want) {
		t.Errorf("changed %v, want %v", paths(changed), want)
	}
	// A key without a file ("old") is not reported: there is nothing to do with it.
	if all := diffBucket(cur, nil, "p/"); len(all) != 4 {
		t.Errorf("against an empty bucket: %v", paths(all))
	}
}

func TestInventoryFind(t *testing.T) {
	i := inv("a", 1, 1, "b/c", 2, 2, "z", 3, 3)
	if f, ok := i.find("b/c"); !ok || f.Size != 2 {
		t.Errorf("find b/c = %+v %v", f, ok)
	}
	if _, ok := i.find("b"); ok {
		t.Error("found b")
	}
	if n, b := i.totals(); n != 3 || b != 6 {
		t.Errorf("totals %d %d", n, b)
	}
}

func TestLimiterPacesTheCopy(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(0, 0)
	var slept []time.Duration
	l := &limiter{rate: 1000, now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			slept = append(slept, d)
			now = now.Add(d)
			mu.Unlock()
			return nil
		}}
	for _, n := range []int64{500, 500, 2000, 0, 100} {
		if err := l.wait(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	// The first goes at once; each later one waits for the time the earlier bytes take.
	want := []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}
	if !reflect.DeepEqual(slept, want) {
		t.Errorf("slept %v, want %v", slept, want)
	}
	var none *limiter
	if err := none.wait(context.Background(), 1<<30); err != nil {
		t.Error(err)
	}
}

func TestWalkSkipsLinksAndIgnoresAMissingRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "b", "n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b", "n", "v1"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "b", "link")); err != nil {
		t.Fatal(err)
	}
	var got, skipped []string
	err := OSFiles{}.Walk(context.Background(), dir, func(abs string) { skipped = append(skipped, filepath.Base(abs)) },
		func(o Object) error { got = append(got, o.Path); return nil })
	if err != nil || !reflect.DeepEqual(got, []string{"b/n/v1"}) || !reflect.DeepEqual(skipped, []string{"link"}) {
		t.Errorf("walk: %v %v %v", got, skipped, err)
	}
	if err := (OSFiles{}).Walk(context.Background(), filepath.Join(dir, "none"), nil, func(Object) error { t.Error("called"); return nil }); err != nil {
		t.Errorf("a missing root: %v", err)
	}
	if _, err := openNoFollow(filepath.Join(dir, "b", "link")); err == nil {
		t.Error("openNoFollow followed a link")
	}
}

func TestExtendedAttributesRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	ok, err := OSFiles{}.SetMeta(p, FileMeta{ContentType: "image/png", CacheControl: "max-age=1"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("this file system has no extended attributes")
	}
	m, err := OSFiles{}.Meta(p)
	if err != nil || m.ContentType != "image/png" || m.CacheControl != "max-age=1" {
		t.Errorf("Meta = %+v, %v", m, err)
	}
	if _, err := (OSFiles{}).Meta(filepath.Join(t.TempDir(), "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Meta of a missing file: %v", err)
	}
	if m, err := (OSFiles{}).Meta(p); err != nil || m.ContentType == "" {
		t.Errorf("Meta = %+v, %v", m, err)
	}
	other := filepath.Join(t.TempDir(), "g")
	if err := os.WriteFile(other, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if m, err := (OSFiles{}).Meta(other); err != nil || m != (FileMeta{}) {
		t.Errorf("Meta of a file without attributes = %+v, %v", m, err)
	}
}
