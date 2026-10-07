//go:build !unix

package artifacts

func handOver(string, string) error { return nil }
