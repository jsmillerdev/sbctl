package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jsmillerdev/supavise/internal/lifecycle"
)

func TestPrintSizesShowsWhatFitsAndWhy(t *testing.T) {
	var offers []lifecycle.Offer
	for _, c := range lifecycle.Classes() {
		o := lifecycle.Offer{Class: c, Fits: c.MemoryBytes <= 4<<30, Current: c.Name == "micro"}
		if !o.Fits {
			o.Reason = "needs a memory cap of " + memoryText(c)
		}
		offers = append(offers, o)
	}
	cp := lifecycle.Capacity{Node: lifecycle.NodeResources{MemoryBytes: 8 << 30, CPUs: 4}, Overcommit: 3, BudgetBytes: 24 << 30, CommittedBytes: 3 << 30, Projects: 2}
	var b bytes.Buffer
	printSizes(&b, offers, cp, true, "")
	out := b.String()
	for _, want := range []string{
		"SIZE", "nano", "micro   ", "current", "2xlarge", "16xlarge",
		"1 vCPU shared", "2 vCPU dedicated",
		"node: 8.0 GB memory x 3 overcommit = 24.0 GB of project memory caps, 3.0 GB promised to 2 projects, 21.0 GB free; 4 cores",
		"  xlarge: needs a memory cap of 16 GB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "  medium:") || strings.Contains(out, "  micro:") {
		t.Errorf("a size that fits was explained:\n%s", out)
	}
	// Every line of the table says whether the size is available.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "xlarge ") && !strings.HasSuffix(strings.TrimSpace(line), "no") {
			t.Errorf("xlarge line = %q", line)
		}
	}
	views := sizesJSON(offers)
	if len(views) != len(offers) || !views[1].Current || views[5].Available || views[5].Reason == "" {
		t.Errorf("json view = %+v", views)
	}

	b.Reset()
	printSizes(&b, offers, lifecycle.Capacity{}, false, "")
	if !strings.Contains(b.String(), "unknown here") {
		t.Errorf("unknown node: %s", b.String())
	}
}

func TestMemoryText(t *testing.T) {
	for name, want := range map[string]string{"nano": "512 MB", "micro": "1 GB", "16xlarge": "256 GB"} {
		c, _ := lifecycle.ClassFor(name)
		if got := memoryText(c); got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
}
