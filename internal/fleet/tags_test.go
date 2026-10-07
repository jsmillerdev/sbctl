package fleet

import "testing"

func TestTagFromScript(t *testing.T) {
	const dir = "/var/lib/supavise/artifacts"
	script := "#!/bin/sh\nset -a\n. '/var/lib/supavise/projects/system/realtime.env'\ncd '/var/lib/supavise/projects/system/realtime'\nexec '/var/lib/supavise/artifacts/realtime/realtime-v2.140.10-r0/bin/server'\n"
	got, err := tagFromScript(dir, "realtime", script)
	if err != nil || got != "realtime-v2.140.10-r0" {
		t.Fatalf("tagFromScript = %q, %v", got, err)
	}
	// Another service's artifact in the same script is not this service's.
	if _, err := tagFromScript(dir, "storage", script); err == nil {
		t.Fatal("found a storage artifact in the realtime script")
	}
	// A name that is a prefix of another does not match it.
	if _, err := tagFromScript(dir, "real", script); err == nil {
		t.Fatal("matched a service by a prefix of its name")
	}
	// Unquoted, as the launcher of a service with a plain path may be written.
	got, err = tagFromScript(dir, "auth", "exec /var/lib/supavise/artifacts/auth/auth-v2.195.0-r1/bin/auth\n")
	if err != nil || got != "auth-v2.195.0-r1" {
		t.Fatalf("unquoted: %q, %v", got, err)
	}
}
