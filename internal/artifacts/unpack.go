package artifacts

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// maxUnpackBytes bounds the bytes written by one unpack; the largest artifact
// (postgres) is about 0.5 GiB unpacked.
const maxUnpackBytes = 8 << 30

// UnpackStats summarises one unpack.
type UnpackStats struct {
	Files, Dirs, Symlinks int
	Bytes                 int64
}

// Unpack decompresses a tar.zst stream from r into dest, which must exist. It
// refuses absolute names, ".." components, links that leave dest, writes through
// symlinked parents and device or FIFO entries. File and directory permission bits
// are kept; setuid, setgid and sticky bits are dropped. Symlinks and hard links are
// preserved.
func Unpack(r io.Reader, dest string) (UnpackStats, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return UnpackStats{}, fmt.Errorf("artifacts: zstd: %w", err)
	}
	defer zr.Close()
	return extractTar(tar.NewReader(zr), dest)
}

func extractTar(tr *tar.Reader, dest string) (UnpackStats, error) {
	var st UnpackStats
	symlinks := map[string]bool{} // cleaned relative paths of extracted symlinks
	linkTargets := map[string]string{}
	dirModes := map[string]os.FileMode{}

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return st, fmt.Errorf("artifacts: read tar: %w", err)
		}
		rel, err := cleanEntryName(hdr.Name)
		if err != nil {
			return st, err
		}
		if rel == "" { // the archive root "./"
			continue
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if hasSymlinkParent(rel, symlinks) {
			return st, fmt.Errorf("artifacts: entry %q is written through a symlink", hdr.Name)
		}
		if symlinks[rel] && hdr.Typeflag != tar.TypeSymlink {
			return st, fmt.Errorf("artifacts: entry %q replaces a symlink", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		perm := os.FileMode(hdr.Mode) & 0o777

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Keep directories writable while extracting; final modes are applied last.
			if err := os.MkdirAll(target, perm|0o700); err != nil {
				return st, err
			}
			dirModes[target] = perm
			st.Dirs++
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return st, err
			}
			if st.Bytes+hdr.Size > maxUnpackBytes {
				return st, fmt.Errorf("artifacts: archive expands beyond %d bytes", int64(maxUnpackBytes))
			}
			if err := writeFile(target, tr, perm); err != nil {
				return st, err
			}
			st.Files++
			st.Bytes += hdr.Size
		case tar.TypeSymlink:
			if err := checkLinkTarget(rel, hdr.Linkname); err != nil {
				return st, err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return st, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return st, err
			}
			symlinks[rel] = true
			linkTargets[rel] = hdr.Linkname
			st.Symlinks++
		case tar.TypeLink:
			src, err := cleanEntryName(hdr.Linkname)
			if err != nil || src == "" {
				return st, fmt.Errorf("artifacts: hard link %q has a bad target %q", hdr.Name, hdr.Linkname)
			}
			if hasSymlinkParent(src, symlinks) || symlinks[src] {
				return st, fmt.Errorf("artifacts: hard link %q targets a symlink", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return st, err
			}
			_ = os.Remove(target)
			if err := os.Link(filepath.Join(dest, filepath.FromSlash(src)), target); err != nil {
				return st, err
			}
			st.Files++
		default:
			return st, fmt.Errorf("artifacts: entry %q has unsupported type %q", hdr.Name, string(hdr.Typeflag))
		}
	}

	// The per-entry check above is lexical. Now that every link exists, resolve each
	// through the others so that a chain such as s -> . and l -> s/.. cannot point out
	// of the artifact.
	for rel, name := range linkTargets {
		if err := resolveLink(rel, name, linkTargets); err != nil {
			return st, err
		}
	}

	// Apply directory modes deepest first so a read-only parent does not block its children.
	dirs := make([]string, 0, len(dirModes))
	for d := range dirModes {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := os.Chmod(d, dirModes[d]); err != nil {
			return st, err
		}
	}
	return st, nil
}

// cleanEntryName returns the slash-separated, root-relative form of a tar name, ""
// for the archive root, or an error for names that could escape it.
func cleanEntryName(name string) (string, error) {
	if strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("artifacts: unsafe entry name %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("artifacts: unsafe entry name %q", name)
		}
	}
	c := path.Clean(name)
	if c == "." {
		return "", nil
	}
	return c, nil
}

// checkLinkTarget rejects symlinks whose target is absolute or resolves outside the
// archive root when the link sits at rel.
func checkLinkTarget(rel, linkname string) error {
	if linkname == "" || strings.ContainsRune(linkname, 0) || path.IsAbs(linkname) {
		return fmt.Errorf("artifacts: symlink %q has an unsafe target %q", rel, linkname)
	}
	resolved := path.Clean(path.Join(path.Dir(rel), linkname))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("artifacts: symlink %q escapes the artifact: %q", rel, linkname)
	}
	return nil
}

// maxLinkHops bounds symlink expansion, like the kernel's limit, so loops end.
const maxLinkHops = 40

// resolveLink follows the symlink at rel (target name) through the other symlinks in
// links, as the kernel would, and fails if it leaves the artifact root or loops.
func resolveLink(rel, name string, links map[string]string) error {
	stack := []string{}
	if d := path.Dir(rel); d != "." {
		stack = strings.Split(d, "/")
	}
	queue := strings.Split(name, "/")
	hops := 0
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return fmt.Errorf("artifacts: symlink %q escapes the artifact through other links: %q", rel, name)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, c)
		if t, ok := links[strings.Join(stack, "/")]; ok {
			if hops++; hops > maxLinkHops {
				return fmt.Errorf("artifacts: symlink %q loops or nests too deeply: %q", rel, name)
			}
			stack = stack[:len(stack)-1]
			queue = append(strings.Split(t, "/"), queue...)
		}
	}
	return nil
}

func hasSymlinkParent(rel string, symlinks map[string]bool) bool {
	for p := path.Dir(rel); p != "." && p != "/"; p = path.Dir(p) {
		if symlinks[p] {
			return true
		}
	}
	return false
}

func writeFile(target string, r io.Reader, perm os.FileMode) error {
	_ = os.Remove(target)
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(target, perm)
}
