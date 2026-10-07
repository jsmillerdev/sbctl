//go:build !linux

package main

// hardenProcess is a no-op off Linux, where supavise only runs as a development build.
func hardenProcess() {}
