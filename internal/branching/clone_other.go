//go:build !darwin && !linux

package branching

const cloneMethodName = MethodReflink

func cloneFile(src, dst string) error { return errNoClone }

func fsName(path string) string { return "unknown" }
