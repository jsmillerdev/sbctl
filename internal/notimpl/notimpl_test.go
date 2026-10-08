package notimpl

import (
	"errors"
	"testing"
)

func TestFor(t *testing.T) {
	err := For("supavise node join")
	if !errors.Is(err, Err) || err.Error() != "supavise node join: not implemented yet" {
		t.Fatalf("For = %v", err)
	}
}
