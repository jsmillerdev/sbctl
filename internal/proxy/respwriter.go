package proxy

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
)

// statusWriter records the status and size of a response. It keeps Flush and
// Hijack working: WebSocket upgrades hijack the connection.
type statusWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return c, rw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// code is the status to log: 101 for a hijacked (upgraded) connection.
func (w *statusWriter) code() int {
	switch {
	case w.hijacked && w.status == 0:
		return http.StatusSwitchingProtocols
	case w.status == 0:
		return http.StatusOK
	}
	return w.status
}

func slogLevel(status int) slog.Level {
	if status >= 500 {
		return slog.LevelWarn
	}
	return slog.LevelDebug
}
