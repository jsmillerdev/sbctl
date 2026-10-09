//go:build unix

package main

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The drop to the supavise user sets the groups and the gid before the uid: after setuid the process may
// change neither, and it would stay in root's groups. A call that fails stops the drop, so the command
// stops instead of seeding a data directory as root.
func TestDropToSetsGroupsAndGidBeforeUid(t *testing.T) {
	var calls []string
	cred := &syscall.Credential{Uid: 996, Gid: 995, Groups: []uint32{995, 1001}}
	var failing string
	id := func(name string) func(int) error {
		return func(n int) error {
			calls = append(calls, name+":"+strconv.Itoa(n))
			if failing == name {
				return errors.New("operation not permitted")
			}
			return nil
		}
	}
	groups := func(gs []int) error {
		calls = append(calls, "groups:"+strconv.Itoa(gs[0])+","+strconv.Itoa(gs[1]))
		if failing == "groups" {
			return errors.New("operation not permitted")
		}
		return nil
	}

	if err := dropTo(cred, groups, id("gid"), id("uid")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"groups:995,1001", "gid:995", "uid:996"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}

	// A call that fails ends the drop there.
	for i, name := range []string{"groups", "gid", "uid"} {
		calls, failing = nil, name
		err := dropTo(cred, groups, id("gid"), id("uid"))
		if err == nil || !strings.Contains(err.Error(), "becoming the supavise user") || !strings.Contains(err.Error(), "operation not permitted") {
			t.Fatalf("a failing %s call = %v", name, err)
		}
		if len(calls) != i+1 {
			t.Fatalf("calls after a failing %s call: %v", name, calls)
		}
	}
}
