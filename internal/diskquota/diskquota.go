// Package diskquota knows what disk a project may use: the data volume's size and usage, and
// the per-project limit that an XFS filesystem mounted with project quotas (prjquota) can
// enforce. Anywhere else a project's disk size is informational, because nothing stops a
// cluster from filling the volume it shares with the other projects.
//
// Setting an XFS project quota needs root (xfs_quota). The daemon runs as the supavise user, so
// it writes the wanted limit next to the project (Setting) and starts the root one-shot unit
// supavise-diskquota@<ref>, which runs `supavise system set-disk-quota <ref>` and calls Apply.
package diskquota

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// BaseProjectID is added to a project's registry sequence number to make its XFS project id.
// Ids below it are left to the administrator.
const BaseProjectID = 100000

// ProjectID is the XFS project id of the project with the registry sequence number seq.
func ProjectID(seq int) uint32 { return BaseProjectID + uint32(max(seq, 0)) }

// Volume describes the filesystem that holds a directory.
type Volume struct {
	// Mount is the mount point of the filesystem.
	Mount  string
	FSType string
	// Options are the mount options (the mount's and the superblock's together).
	Options []string
	// TotalBytes and FreeBytes are what the filesystem reports (free: available to the supavise user).
	TotalBytes, FreeBytes int64
}

// Enforceable reports whether project quotas can limit a directory of the volume: XFS, mounted
// with prjquota (or its alias pquota) and without pqnoenforce.
func (v Volume) Enforceable() bool {
	if v.FSType != "xfs" {
		return false
	}
	on := false
	for _, o := range v.Options {
		switch o {
		case "prjquota", "pquota":
			on = true
		case "pqnoenforce", "noquota":
			return false
		}
	}
	return on
}

// Inspect describes the volume holding dir. It works up to the nearest directory that exists, so
// it can be asked before the first project has been created. On a system without
// /proc/self/mountinfo the filesystem type and options stay empty and nothing is enforceable.
func Inspect(dir string) (Volume, error) {
	d := dir
	for {
		if _, err := os.Stat(d); err == nil {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			return Volume{}, fmt.Errorf("diskquota: %s does not exist", dir)
		}
		d = parent
	}
	if real, err := filepath.EvalSymlinks(d); err == nil {
		d = real
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(d, &st); err != nil {
		return Volume{}, err
	}
	v := Volume{TotalBytes: int64(st.Blocks) * int64(st.Bsize), FreeBytes: int64(st.Bavail) * int64(st.Bsize)}
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		defer f.Close()
		v.Mount, v.FSType, v.Options = parseMountInfo(f, d)
	}
	return v, nil
}

// parseMountInfo finds the mount that holds dir in the contents of /proc/self/mountinfo: the one
// with the longest mount point that is a parent of dir (the last listed wins on a tie, as the
// kernel shows an overmount after what it covers).
func parseMountInfo(r io.Reader, dir string) (mount, fstype string, opts []string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		f := strings.Fields(pre)
		g := strings.Fields(post)
		if len(f) < 6 || len(g) < 3 {
			continue
		}
		mp := unescapeMount(f[4])
		if !within(mp, dir) || len(mp) < len(mount) {
			continue
		}
		mount, fstype = mp, g[0]
		opts = append(strings.Split(f[5], ","), strings.Split(g[2], ",")...)
	}
	return mount, fstype, opts
}

// within reports whether dir is mp or below it.
func within(mp, dir string) bool {
	if mp == "/" {
		return true
	}
	return dir == mp || strings.HasPrefix(dir, mp+"/")
}

// unescapeMount undoes the octal escapes of mountinfo (a space is \040).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Used is the number of bytes of the regular files under dir (0 when it does not exist).
func Used(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Setting is the limit asked for a project: what the daemon writes and the root unit reads.
type Setting struct {
	ProjectID uint32
	SizeGB    int
}

// SettingFile is where a project's Setting lives.
func SettingFile(projectDir string) string { return filepath.Join(projectDir, "disk-quota") }

// WriteSetting stores s in path (0640: the root unit reads it, other users do not need to).
func WriteSetting(path string, s Setting) error {
	if err := s.Validate(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("project_id=%d\nsize_gb=%d\n", s.ProjectID, s.SizeGB)), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadSetting reads the file WriteSetting wrote; ok is false when there is none.
func ReadSetting(path string) (s Setting, ok bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Setting{}, false, nil
	}
	if err != nil {
		return Setting{}, false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		n, perr := strconv.ParseUint(v, 10, 32)
		switch k {
		case "project_id":
			s.ProjectID = uint32(n)
		case "size_gb":
			s.SizeGB = int(n)
		default:
			continue
		}
		if perr != nil {
			return Setting{}, false, fmt.Errorf("diskquota: %s: bad %s", path, k)
		}
	}
	return s, true, s.Validate()
}

// Validate checks what the root unit relies on: it passes these numbers to xfs_quota.
func (s Setting) Validate() error {
	if s.ProjectID < BaseProjectID || s.ProjectID > 4_000_000_000 {
		return fmt.Errorf("diskquota: project id %d is outside the range Supavise uses", s.ProjectID)
	}
	if s.SizeGB < 1 || s.SizeGB > 1<<16 {
		return fmt.Errorf("diskquota: size %d GB is outside 1 to 65536", s.SizeGB)
	}
	return nil
}

// Runner runs a program and returns its combined output. exec is the default.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec is the Runner that starts a program.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Apply marks dir with the project id and sets the project's hard block limit on the XFS
// filesystem mounted at mount. It needs root (xfs_quota) and a mount with prjquota. The marking
// is recursive and inherited by files created later, so the limit covers the whole tree.
func Apply(ctx context.Context, run Runner, mount, dir string, s Setting) error {
	if err := s.Validate(); err != nil {
		return err
	}
	id := strconv.FormatUint(uint64(s.ProjectID), 10)
	for _, cmd := range []string{
		"project -s -p " + dir + " " + id,
		fmt.Sprintf("limit -p bhard=%dg %s", s.SizeGB, id),
	} {
		if out, err := run(ctx, "xfs_quota", "-x", "-c", cmd, mount); err != nil {
			return fmt.Errorf("diskquota: xfs_quota %q on %s: %w: %s", cmd, mount, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
