//go:build !linux

package main

// hardenProcess is a no-op off Linux, where sbctl only runs as a development build.
func hardenProcess() {}
