// Command mock is a stand-in for the supavise Management API, for the Studio spike. It serves the
// /platform, /v1 and /v2 routes Studio calls, with two projects, logs every request as JSON
// lines, validates the dashboard's GoTrue access tokens, proxies pg-meta queries to a real
// postgres-meta, and answers every other documented route with an empty value of the shape the
// OpenAPI specs describe.
//
//	mock serve -config mock.json        run the server
//	mock summarize request-log.jsonl    print the calls seen as a markdown table
//
// It is a test tool, not the API server (workstream B owns that); it shares no code with it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jsmillerdev/supavise/internal/secrets"
)

// Config is the JSON file read by `mock serve -config`.
type Config struct {
	// Listen is the address to serve on, for example 127.0.0.1:8081.
	Listen string `json:"listen"`
	// JWTSecret verifies the HS256 access tokens GoTrue issues for the dashboard (GOTRUE_JWT_SECRET).
	JWTSecret string `json:"jwt_secret"`
	// StudioOrigins are the browser origins allowed by CORS, for example http://127.0.0.1:3000.
	StudioOrigins []string `json:"studio_origins"`
	// PgmetaURL is the base URL of a running postgres-meta, for example http://127.0.0.1:8080.
	PgmetaURL string `json:"pgmeta_url"`
	// PgmetaCryptoKey is postgres-meta's CRYPTO_KEY; connection strings are AES-encrypted with it.
	PgmetaCryptoKey string `json:"pgmeta_crypto_key"`
	// Scheme and ProjectHost build project URLs: <scheme>://<ref>.<project_host>.
	Scheme      string `json:"scheme"`
	ProjectHost string `json:"project_host"`
	// RequestLog is the JSON-lines file every request is appended to ("" disables it).
	RequestLog string `json:"request_log"`
	// BuiltinAuth serves a minimal GoTrue-compatible token endpoint under /auth/v1, so the mock
	// can run without a GoTrue process (unit tests, quick checks). Leave nil to use real GoTrue.
	BuiltinAuth *BuiltinAuth `json:"builtin_auth"`
	// Projects are served in the order given. There must be at least one.
	Projects []Project `json:"projects"`
}

// BuiltinAuth is the single user of the built-in token endpoint.
type BuiltinAuth struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Project is one project the mock serves.
type Project struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
	// DBURL is the postgres:// URL pg-meta connects to for this project.
	DBURL string `json:"db_url"`
	// JWTSecret signs the project's anon and service_role keys (random per run when empty).
	JWTSecret string `json:"jwt_secret"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8081"
	}
	if c.Scheme == "" {
		c.Scheme = "http"
	}
	if c.ProjectHost == "" {
		c.ProjectHost = "localhost"
	}
	if c.JWTSecret == "" {
		return errors.New("jwt_secret is required")
	}
	if len(c.Projects) == 0 {
		return errors.New("at least one project is required")
	}
	seen := map[string]bool{}
	for i := range c.Projects {
		p := &c.Projects[i]
		if !secrets.ValidRef(p.Ref) {
			return fmt.Errorf("project %d: ref %q is not 20 lowercase letters", i, p.Ref)
		}
		if seen[p.Ref] {
			return fmt.Errorf("duplicate ref %s", p.Ref)
		}
		seen[p.Ref] = true
		if p.Name == "" {
			p.Name = p.Ref
		}
		if p.JWTSecret == "" {
			p.JWTSecret = secrets.NewJWTSecret()
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serveCmd(os.Args[2:]))
	case "summarize":
		os.Exit(summarizeCmd(os.Args[2:]))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mock serve -config mock.json | mock summarize request-log.jsonl")
	os.Exit(2)
}

func serveCmd(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to the JSON config")
	listen := fs.String("listen", "", "override the listen address")
	_ = fs.Parse(args)
	if *cfgPath == "" {
		usage()
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Printf("mock: %v", err)
		return 1
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	srv, err := newServer(cfg)
	if err != nil {
		log.Printf("mock: %v", err)
		return 1
	}
	defer srv.close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Printf("mock: %v", err)
		return 1
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mock: serving %d projects on http://%s (request log: %q)", len(cfg.Projects), ln.Addr(), cfg.RequestLog)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- httpSrv.Serve(ln) }()
	select {
	case err := <-done:
		log.Printf("mock: %v", err)
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	return 0
}
