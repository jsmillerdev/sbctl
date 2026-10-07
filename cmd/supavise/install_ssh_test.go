package main

import (
	"reflect"
	"testing"
)

func TestParseListeningSSHPorts(t *testing.T) {
	ss := `LISTEN 0 4096 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=812,fd=3))
LISTEN 0 4096    [::]:2222    [::]:* users:(("sshd",pid=812,fd=4))
LISTEN 0 4096 127.0.0.54:53 0.0.0.0:* users:(("systemd-resolve",pid=500,fd=17))
LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=900,fd=5))
`
	got := uniqueSorted(parseListeningSSHPorts(ss))
	if !reflect.DeepEqual(got, []int{2222}) {
		t.Fatalf("%v", got)
	}
}

func TestParseSocketListenAndConfig(t *testing.T) {
	// Ubuntu 24.04 socket-activated ssh: sshd_config's Port is ignored, ssh.socket decides.
	got := uniqueSorted(parseSocketListen("[::]:2200 (Stream)\n0.0.0.0:2200 (Stream)\n"))
	if !reflect.DeepEqual(got, []int{2200}) {
		t.Fatalf("socket: %v", got)
	}
	cfg := "# Port 22\nPort 2022\n  port 2023\nPasswordAuthentication no\n"
	if got := uniqueSorted(parseConfiguredSSHPorts(cfg)); !reflect.DeepEqual(got, []int{2022, 2023}) {
		t.Fatalf("config: %v", got)
	}
	if got := parseConfiguredSSHPorts("# Port 22\n"); len(got) != 0 {
		t.Fatalf("a commented Port counted: %v", got)
	}
}
