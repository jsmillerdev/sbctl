// Package systemd embeds the unit templates, the slice and the polkit rule so the
// binary can install them (supavise system converge, and its alias install-units) without the
// repository.
package systemd

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed *.service *.slice *.rules *.timer
var files embed.FS

// Names lists the embedded file names.
func Names() []string {
	ents, _ := fs.ReadDir(files, ".")
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// Read returns one embedded file.
func Read(name string) ([]byte, error) { return files.ReadFile(name) }

// Install writes every unit and the slice into unitDir (normally /etc/systemd/system)
// and the polkit rule into polkitDir (normally /etc/polkit-1/rules.d; "" skips it).
// It returns the files whose content changed, so the caller can daemon-reload only
// when something did.
func Install(unitDir, polkitDir string) (changed []string, err error) {
	return InstallWith(unitDir, polkitDir, nil)
}

// InstallWith is Install with per-file content overrides, keyed by embedded file name.
// A node whose config differs from an embedded default (the base backup schedule) passes
// its rendering here, so the file is written once with its final content and a second
// run reports no change.
func InstallWith(unitDir, polkitDir string, override map[string][]byte) (changed []string, err error) {
	todo, err := plan(unitDir, polkitDir, override)
	if err != nil {
		return nil, err
	}
	for _, f := range todo {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return changed, err
		}
		if err := os.WriteFile(f.path, f.content, 0o644); err != nil {
			return changed, err
		}
		changed = append(changed, f.path)
	}
	return changed, nil
}

// Pending returns the files InstallWith would write, and writes nothing.
func Pending(unitDir, polkitDir string, override map[string][]byte) ([]string, error) {
	todo, err := plan(unitDir, polkitDir, override)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(todo))
	for i, f := range todo {
		paths[i] = f.path
	}
	return paths, nil
}

type pendingFile struct {
	path    string
	content []byte
}

// plan lists the files whose content differs from what is installed, in the order of Names.
func plan(unitDir, polkitDir string, override map[string][]byte) ([]pendingFile, error) {
	var todo []pendingFile
	for _, name := range Names() {
		dir := unitDir
		if filepath.Ext(name) == ".rules" {
			if polkitDir == "" {
				continue
			}
			dir = polkitDir
		}
		b, ok := override[name]
		if !ok {
			var err error
			if b, err = files.ReadFile(name); err != nil {
				return nil, err
			}
		}
		dst := filepath.Join(dir, name)
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(b) {
			continue
		}
		todo = append(todo, pendingFile{dst, b})
	}
	return todo, nil
}
