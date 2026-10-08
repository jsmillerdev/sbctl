// Package notimpl is the error a stub returns until its workstream fills it in: a command, a hook
// or a function that is registered so that the rest of the code compiles and the help lists it,
// but does nothing yet. One sentinel lets a caller that can live without the feature skip it
// with errors.Is, and the text tells a person what happened.
package notimpl

import (
	"errors"
	"fmt"
)

// Err is wrapped by every error For returns.
var Err = errors.New("not implemented yet")

// For returns the error of the stub named what, for example "supavise node join".
func For(what string) error { return fmt.Errorf("%s: %w", what, Err) }
