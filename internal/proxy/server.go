package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/OWNER/sbctl/internal/config"
)

// Server is sbctl's public edge. Create it with New, then call Run (or Serve with
// listeners you own). Handler exposes the routing without any listener, for tests.
type Server struct {
	opts  Options
	cfg   *config.Config
	log   *slog.Logger
	table *table
	waker Waker

	apiHost, studioHost string

	tlsMode  string
	provider certmagic.DNSProvider

	// upstreamFn overrides the upstream address resolution (tests).
	upstreamFn func(service, project) string

	// sockets are the Realtime sockets opened without inspection (see wsguard.go).
	sockets socketSet
	// lp are the Realtime long-poll sessions opened without inspection (see legacyguard.go).
	lp lpSessions
	// recheckWait is the delay before a failed key lookup is retried (tests); zero is the default.
	recheckWait time.Duration

	mu         sync.Mutex
	transports map[time.Duration]*http.Transport
}

// New validates opts, resolves the TLS strategy and loads the host table once, so
// a broken registry or TLS configuration fails at startup.
func New(opts Options) (*Server, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	s := &Server{
		opts: opts, cfg: opts.Config, log: opts.Logger, waker: opts.Waker,
		transports: map[time.Duration]*http.Transport{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.waker == nil {
		s.waker = noopWaker{}
	}
	if base := s.cfg.BaseDomain(); base != "" {
		s.apiHost, s.studioHost = s.cfg.APIHost(), s.cfg.StudioHost()
	}
	mode, err := resolveTLSMode(s.cfg)
	if err != nil {
		return nil, err
	}
	s.tlsMode = mode
	if mode == tlsDNS01 || mode == tlsAuto {
		if s.provider, err = newDNSProvider(s.cfg.TLS.DNSProvider, s.cfg.TLS.Credentials); err != nil {
			return nil, err
		}
	}
	s.table = newTable(s.cfg, opts.Registry, opts.Keys, s.log)
	s.table.onKeysDropped = func(ref string) {
		if len(s.sockets.refs(ref)) > 0 || len(s.lp.refs(ref)) > 0 {
			go s.recheckRealtime(ref)
		}
	}
	if err := s.table.reload(context.Background()); err != nil {
		return nil, fmt.Errorf("proxy: loading routes: %w", err)
	}
	return s, nil
}

// Handler is the routing handler served on both listeners (plain HTTP in mode off).
func (s *Server) Handler() http.Handler { return s }

// Sync keeps the host and key caches current from registry change notifications
// until ctx ends. Run starts it; call it yourself only when using Handler alone.
func (s *Server) Sync(ctx context.Context) { s.table.sync(ctx) }

// Run listens on Listen.HTTP and Listen.HTTPS and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	httpLn, err := net.Listen("tcp", s.cfg.Listen.HTTP)
	if err != nil {
		return fmt.Errorf("proxy: listen http: %w", err)
	}
	httpsLn, err := net.Listen("tcp", s.cfg.Listen.HTTPS)
	if err != nil {
		httpLn.Close()
		return fmt.Errorf("proxy: listen https: %w", err)
	}
	return s.Serve(ctx, httpLn, httpsLn)
}

// Serve serves on the given listeners until ctx ends, then shuts down gracefully.
// In TLS modes httpLn answers ACME HTTP-01 and redirects to https; in mode off
// both listeners serve the proxy over plain HTTP.
func (s *Server) Serve(ctx context.Context, httpLn, httpsLn net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Sync(ctx)

	var (
		cm         *certManager
		httpH      http.Handler = s
		httpsSrv                = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug)}
		httpSrv                 = &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: httpsSrv.ErrorLog}
		httpsPort               = portOf(httpsLn)
		useTLSOnLn              = false
	)
	if s.tlsMode != tlsOff {
		var err error
		cm, err = newCertManager(certOptions{
			cfg: s.cfg, mode: s.tlsMode, provider: s.provider, allow: s.allowHost,
			httpPort: portOf(httpLn), httpsPort: httpsPort, log: s.log,
		})
		if err != nil {
			// No server owns the listeners yet; do not leak them.
			httpLn.Close()
			httpsLn.Close()
			return err
		}
		defer cm.close()
		httpH = s.redirectHandler(cm, httpsPort)
		httpsSrv.TLSConfig = cm.tlsConfig()
		useTLSOnLn = true
	}
	httpSrv.Handler = httpH

	errc := make(chan error, 2)
	go func() { errc <- serveErr(httpSrv.Serve(httpLn)) }()
	go func() {
		if useTLSOnLn {
			errc <- serveErr(httpsSrv.ServeTLS(httpsLn, "", ""))
		} else {
			errc <- serveErr(httpsSrv.Serve(httpsLn))
		}
	}()
	if cm != nil {
		// After the listeners are up, so HTTP-01 and TLS-ALPN-01 challenges can be answered.
		if err := cm.manage(ctx); err != nil {
			s.log.Error("proxy: certificate management failed to start", "err", err)
		}
	}
	s.log.Info("proxy listening", "http", httpLn.Addr().String(), "https", httpsLn.Addr().String(), "tls", s.tlsMode,
		"domain", s.cfg.BaseDomain())

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	_ = httpSrv.Shutdown(sctx)
	_ = httpsSrv.Shutdown(sctx)
	// Shutdown leaves hijacked WebSocket connections alone; they end with the process.
	s.closeTransports()
	return runErr
}

func (s *Server) closeTransports() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.transports {
		t.CloseIdleConnections()
	}
}

func serveErr(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func portOf(ln net.Listener) int {
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}
