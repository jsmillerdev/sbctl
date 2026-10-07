package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestAPIProfile(t *testing.T) {
	t.Setenv("SBCTL_DOMAIN", "example.dev")
	t.Setenv("SBCTL_CONFIG", "/nonexistent/config.toml")
	for format, want := range map[string]string{
		"yaml": "project_host: api.example.dev\n",
		"toml": `project_host = "api.example.dev"`,
		"json": `"project_host": "api.example.dev"`,
	} {
		var buf bytes.Buffer
		rootCmd.SetOut(&buf)
		rootCmd.SetArgs([]string{"api", "profile", "--format", format})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if !strings.Contains(buf.String(), want) || !strings.Contains(buf.String(), "https://api.example.dev") {
			t.Errorf("%s output:\n%s", format, buf.String())
		}
	}
}
