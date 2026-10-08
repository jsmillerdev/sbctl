//go:build !linux

package main

import "errors"

func becomeSupavise() error { return errors.New("the supavise user exists on Linux servers only") }
