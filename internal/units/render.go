package units

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/supavise/supavise/internal/config"
	"github.com/supavise/supavise/internal/fsutil"
)

// Files are the paths Render owns for one unit.
type Files struct {
	Env string // EnvironmentFile, 0600
	Run string // launcher script the unit template executes, 0750
}

// Held is the name the launcher takes while a planned stop keeps its unit from starting (Hold).
func (f Files) Held() string { return f.Run + ".held" }

// Hold moves the launcher aside. The unit template requires the launcher to exist
// (ConditionPathExists), so systemd does not start the unit again, not by a dependency that wants
// it (supavise.service wants the system cluster when the daemon restarts) and not at boot. A
// launcher that is not there is not an error: the hold is repeatable.
func (f Files) Hold() error {
	if err := os.Rename(f.Run, f.Held()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("units: holding %s: %w", f.Run, err)
	}
	return nil
}

// Unhold puts a held launcher back. It does nothing when nothing is held, and does not replace a
// launcher that was rendered again in the meantime (that one is newer; the held copy is dropped).
func (f Files) Unhold() error {
	if _, err := os.Stat(f.Held()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("units: %w", err)
	}
	if _, err := os.Stat(f.Run); err == nil {
		_ = os.Remove(f.Held())
		return nil
	}
	if err := os.Rename(f.Held(), f.Run); err != nil {
		return fmt.Errorf("units: releasing the hold of %s: %w", f.Run, err)
	}
	return nil
}

// instanceRef is the directory name a unit's files live under: the project ref, or
// "system" for fleet singletons.
func instanceRef(s Spec) string {
	if s.Ref == "" {
		return config.SystemRef
	}
	return s.Ref
}

// FilesFor returns the env file and run script paths of spec. The systemd templates
// in deploy/systemd execute <state_dir>/projects/<ref>/<svc>.run and read
// <state_dir>/projects/<ref>/<svc>.env.
func FilesFor(cfg *config.Config, s Spec) Files {
	env := cfg.Paths().EnvFile(instanceRef(s), s.Service)
	return Files{Env: env, Run: strings.TrimSuffix(env, ".env") + ".run"}
}

// ParseUnit splits "supavise-<svc>@<ref>.service" or "supavise-<svc>.service" into service and ref
// (ref is "" for singletons).
func ParseUnit(unit string) (svc, ref string, err error) {
	name := strings.TrimSuffix(unit, ".service")
	name, ok := strings.CutPrefix(name, "supavise-")
	if !ok || name == "" {
		return "", "", fmt.Errorf("units: %q is not a supavise unit", unit)
	}
	svc, ref, _ = strings.Cut(name, "@")
	if svc == "" {
		return "", "", fmt.Errorf("units: %q is not a supavise unit", unit)
	}
	return svc, ref, nil
}

// runFilesFor resolves a unit name to its rendered files.
func runFilesFor(cfg *config.Config, unit string) (Files, string, string, error) {
	svc, ref, err := ParseUnit(unit)
	if err != nil {
		return Files{}, "", "", err
	}
	return FilesFor(cfg, Spec{Service: svc, Ref: ref}), svc, ref, nil
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FormatEnv renders env as a systemd EnvironmentFile: sorted KEY="value" lines with
// backslash and double quote escaped, so JSON, spaces and "#" survive. Values must
// not contain newlines or NUL.
func FormatEnv(env map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for _, k := range keys {
		v := env[k]
		if !envKeyRE.MatchString(k) {
			return nil, fmt.Errorf("units: invalid environment variable name %q", k)
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Errorf("units: environment variable %s contains a newline or NUL", k)
		}
		v = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, v)
	}
	return b.Bytes(), nil
}

// ParseEnv reads a file written by FormatEnv with systemd's semantics for the subset
// it produces (the exec backend uses it so both backends see the same environment).
func ParseEnv(b []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envKeyRE.MatchString(k) || len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
			return nil, fmt.Errorf("units: env file line %d is malformed", i+1)
		}
		v = v[1 : len(v)-1]
		var sb strings.Builder
		for j := 0; j < len(v); j++ {
			if v[j] == '\\' && j+1 < len(v) {
				j++
			}
			sb.WriteByte(v[j])
		}
		out[k] = sb.String()
	}
	return out, nil
}

// shellQuote single-quotes s for /bin/sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// FormatRun renders the launcher script: change to the work directory, run the
// PreStart commands, then exec the launcher. Everything is resolved to absolute paths
// so the script does not depend on the caller's environment.
func FormatRun(s Spec) ([]byte, error) {
	if s.ArtifactDir == "" {
		return nil, errors.New("units: spec has no ArtifactDir")
	}
	exe := s.Exec
	if len(exe) == 0 {
		exe = StandardExec(s.Service)
	}
	if len(exe) == 0 {
		return nil, fmt.Errorf("units: no launcher known for service %q", s.Service)
	}
	line := func(args []string) (string, error) {
		if len(args) == 0 || args[0] == "" {
			return "", errors.New("units: empty command")
		}
		if filepath.IsAbs(args[0]) || strings.Contains(args[0], "..") {
			return "", fmt.Errorf("units: command %q must be relative to the artifact", args[0])
		}
		q := make([]string, len(args))
		q[0] = shellQuote(filepath.Join(s.ArtifactDir, args[0]))
		for i, a := range args[1:] {
			q[i+1] = shellQuote(a)
		}
		return strings.Join(q, " "), nil
	}
	var b bytes.Buffer
	b.WriteString("#!/bin/sh\n# Generated by supavise; changes are overwritten.\nset -e\n")
	if s.WorkDir != "" {
		fmt.Fprintf(&b, "cd %s\n", shellQuote(s.WorkDir))
	}
	if s.Log != "" {
		fmt.Fprintf(&b, "exec >%s 2>&1\n", shellQuote(s.Log))
	}
	for _, p := range s.PreStart {
		l, err := line(p)
		if err != nil {
			return nil, err
		}
		b.WriteString(l + "\n")
	}
	l, err := line(exe)
	if err != nil {
		return nil, err
	}
	b.WriteString("exec " + l + "\n")
	return b.Bytes(), nil
}

// writeIfChanged writes b to path with mode atomically unless the file already holds
// exactly b with that mode. It reports whether it wrote.
func writeIfChanged(path string, b []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, b) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == mode {
			return false, nil
		}
	}
	return true, fsutil.WriteFile(path, b, mode, fsutil.Options{MkdirMode: 0o750})
}

// renderFiles writes the env file and run script for s and reports whether either changed.
func renderFiles(cfg *config.Config, s Spec) (changed bool, err error) {
	if s.Service == "" {
		return false, errors.New("units: spec has no Service")
	}
	f := FilesFor(cfg, s)
	env, err := FormatEnv(s.Env)
	if err != nil {
		return false, err
	}
	run, err := FormatRun(s)
	if err != nil {
		return false, err
	}
	a, err := writeIfChanged(f.Env, env, 0o600)
	if err != nil {
		return false, err
	}
	runMode := os.FileMode(0o750)
	if s.PublicRun {
		runMode = 0o755
	}
	b, err := writeIfChanged(f.Run, run, runMode)
	if err != nil {
		return false, err
	}
	return a || b, nil
}

// removeFiles deletes what renderFiles wrote.
func removeFiles(f Files) error {
	var first error
	for _, p := range []string{f.Env, f.Run, f.Held()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// ParseBytes parses a systemd size such as "1G", "512M" or "infinity" (K, M, G and T
// are powers of 1024). It returns ^uint64(0) for infinity and for the empty string.
func ParseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "infinity" {
		return ^uint64(0), nil
	}
	mult := uint64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	var n uint64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || fmt.Sprint(n) != strings.TrimSpace(s) {
		return 0, fmt.Errorf("units: invalid size %q", s)
	}
	return n * mult, nil
}

// ParseCPUQuota converts "150%" to microseconds of CPU time per second (systemd's
// CPUQuotaPerSecUSec); the empty string and "infinity" mean no quota (^uint64(0)).
func ParseCPUQuota(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "infinity" {
		return ^uint64(0), nil
	}
	var pct uint64
	if !strings.HasSuffix(s, "%") {
		return 0, fmt.Errorf("units: invalid CPU quota %q (want e.g. 100%%)", s)
	}
	if _, err := fmt.Sscanf(strings.TrimSuffix(s, "%"), "%d", &pct); err != nil || pct == 0 {
		return 0, fmt.Errorf("units: invalid CPU quota %q", s)
	}
	return pct * 10000, nil
}
