package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jsmillerdev/supavise/internal/secrets"
)

// With the legacy keys disabled, the anon and service_role JWTs must stop working on
// Realtime too. Realtime also takes a JWT inside the socket (the access_token of a phx_join
// payload or of an "access_token" event) and authorizes channels with its claims, which the
// handshake check cannot see. So the proxy watches the client-to-server direction of a
// /realtime/v1 socket for one of the project's legacy keys, and ends the connection when it finds
// one. (Long poll, the other Realtime transport, is refused while the keys are disabled; see
// handler.go.) Everything else streams untouched.
//
// Memory per connection is constant: a frame is never buffered. Its payload is unmasked in a
// small scratch buffer, JSON string escapes are decoded on the fly (so "eyJ..." is the
// same key as "eyJ..."), and a window of the last len(key)-1 decoded bytes is kept so a key
// split across reads, frames or message fragments is still found.

var errLegacyKeyInStream = errors.New("proxy: a legacy API key was sent while the project's legacy keys are disabled")

// legacyNeedles returns the byte strings that give away a legacy key, or nil when the legacy
// keys are enabled. For a JWT it is "<header>.<payload>." of the key: the HMAC input plus the
// dot that starts the signature. Whatever follows (the signature in its canonical spelling, with
// flipped unused low bits, with "=" padding) does not change what the token is, so matching the
// whole key text would let a re-encoded signature through while upstream still accepts it.
func legacyNeedles(k *secrets.ProjectKeys) [][]byte {
	if k == nil || !k.LegacyDisabled {
		return nil
	}
	var n [][]byte
	for _, key := range []string{k.AnonKey, k.ServiceRoleKey} {
		switch in, ok := signingInput(key); {
		case key == "":
		case ok && strings.Count(key, ".") == 2:
			n = append(n, []byte(in+"."))
		default:
			n = append(n, []byte(key))
		}
	}
	return n
}

// keyScanner finds needles in a JSON text stream, decoding string escapes as it goes.
type keyScanner struct {
	needles [][]byte
	keep    int // longest needle - 1
	win     []byte

	esc    int // 0 plain, 1 after a backslash, 2..5 inside \uXXXX (digits read = esc-2)
	uni    int
	chunk  [512]byte
	nchunk int
}

func newKeyScanner(needles [][]byte) *keyScanner {
	s := &keyScanner{needles: needles}
	for _, n := range needles {
		if len(n)-1 > s.keep {
			s.keep = len(n) - 1
		}
	}
	return s
}

// reset forgets the previous message: a key never spans two messages.
func (s *keyScanner) reset() {
	s.win = s.win[:0]
	s.esc, s.uni, s.nchunk = 0, 0, 0
}

// write feeds raw JSON text and reports whether a needle has been completed.
func (s *keyScanner) write(b []byte) bool {
	for _, c := range b {
		if d, ok := s.decode(c); ok {
			s.chunk[s.nchunk] = d
			s.nchunk++
			if s.nchunk == len(s.chunk) && s.flush() {
				return true
			}
		}
	}
	return s.flush()
}

// decode applies JSON string unescaping to one byte; ok is false while an escape is incomplete.
func (s *keyScanner) decode(c byte) (byte, bool) {
	switch {
	case s.esc == 0:
		if c == '\\' {
			s.esc = 1
			return 0, false
		}
		return c, true
	case s.esc == 1:
		s.esc = 0
		switch c {
		case 'u':
			s.esc, s.uni = 2, 0
			return 0, false
		case 'b':
			return '\b', true
		case 'f':
			return '\f', true
		case 'n':
			return '\n', true
		case 'r':
			return '\r', true
		case 't':
			return '\t', true
		}
		return c, true // \" \\ \/ (and, leniently, any other character)
	default:
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			s.esc = 0
			return 0xFF, true
		}
		s.uni = s.uni<<4 | v
		if s.esc++; s.esc == 6 {
			s.esc = 0
			if s.uni < 0x80 {
				return byte(s.uni), true
			}
			return 0xFF, true // not ASCII: cannot be part of a key
		}
		return 0, false
	}
}

func (s *keyScanner) flush() bool {
	if s.nchunk > 0 {
		s.win = append(s.win, s.chunk[:s.nchunk]...)
		s.nchunk = 0
	}
	for _, n := range s.needles {
		if bytes.Contains(s.win, n) {
			return true
		}
	}
	if len(s.win) > s.keep {
		s.win = append(s.win[:0], s.win[len(s.win)-s.keep:]...)
	}
	return false
}

// wsInspector follows the RFC 6455 framing of client-to-server bytes and feeds the payload of
// text messages (single frames or fragments) to a keyScanner. It never changes the bytes.
type wsInspector struct {
	scan *keyScanner

	stage  int // wsHdr, wsExtLen, wsMaskKey, wsPayload
	hdr    [14]byte
	nhdr   int
	need   int // header bytes still to read in this stage
	op     byte
	fin    bool
	masked bool
	mask   [4]byte
	left   uint64 // payload bytes left in this frame
	pos    int    // payload index, for the mask
	inText bool   // inside a fragmented or single text message
	skip   bool   // this frame's payload is not inspected
	bad    string // protocol reason to close, once set
}

const (
	wsHdr = iota
	wsExtLen
	wsMaskKey
	wsPayload
)

func newWSInspector(needles [][]byte) *wsInspector {
	return &wsInspector{scan: newKeyScanner(needles), need: 2}
}

// feed consumes bytes and reports whether a legacy key was found in a text message. reason is
// set when the stream broke a rule the inspection depends on (reserved bits, i.e. an extension
// such as compression that hides the text), which also ends the connection.
func (w *wsInspector) feed(b []byte) (found bool, reason string) {
	for len(b) > 0 {
		switch w.stage {
		case wsPayload:
			n := len(b)
			if uint64(n) > w.left {
				n = int(w.left)
			}
			if !w.skip && w.inspect(b[:n]) {
				return true, ""
			}
			b = b[n:]
			if w.left -= uint64(n); w.left == 0 {
				w.endFrame()
			}
		default:
			w.hdr[w.nhdr] = b[0]
			w.nhdr++
			b = b[1:]
			if w.need--; w.need == 0 {
				if why := w.header(); why != "" {
					return false, why
				}
			}
		}
	}
	return false, ""
}

// header runs when a header stage is complete and starts the next one.
func (w *wsInspector) header() string {
	switch w.stage {
	case wsHdr:
		b0, b1 := w.hdr[0], w.hdr[1]
		if b0&0x70 != 0 {
			return "reserved WebSocket bits are not allowed here"
		}
		w.fin, w.op = b0&0x80 != 0, b0&0x0f
		w.masked = b1&0x80 != 0
		switch l := b1 & 0x7f; l {
		case 126:
			w.stage, w.need = wsExtLen, 2
		case 127:
			w.stage, w.need = wsExtLen, 8
		default:
			w.left = uint64(l)
			w.afterLength()
		}
	case wsExtLen:
		if w.need = 0; w.nhdr == 4 {
			w.left = uint64(binary.BigEndian.Uint16(w.hdr[2:4]))
		} else {
			w.left = binary.BigEndian.Uint64(w.hdr[2:10])
			if w.left>>63 != 0 {
				return "invalid WebSocket frame length"
			}
		}
		w.afterLength()
	case wsMaskKey:
		copy(w.mask[:], w.hdr[w.nhdr-4:w.nhdr])
		w.startPayload()
	}
	return ""
}

func (w *wsInspector) afterLength() {
	if w.masked {
		w.stage, w.need = wsMaskKey, 4
		return
	}
	w.mask = [4]byte{}
	w.startPayload()
}

func (w *wsInspector) startPayload() {
	switch {
	case w.op == 1: // text starts a message
		w.inText = true
		w.scan.reset()
	case w.op == 2: // binary starts a message that is not inspected
		w.inText = false
	}
	w.skip = w.op >= 8 || !w.inText
	w.pos = 0
	if w.left == 0 {
		w.endFrame()
		return
	}
	w.stage = wsPayload
}

func (w *wsInspector) endFrame() {
	if w.fin && w.op < 8 {
		w.inText = false
	}
	w.stage, w.need, w.nhdr = wsHdr, 2, 0
}

// inspect unmasks payload bytes into a scratch buffer and scans them.
func (w *wsInspector) inspect(p []byte) bool {
	var buf [512]byte
	for len(p) > 0 {
		n := copy(buf[:], p)
		for i := 0; i < n; i++ {
			buf[i] = p[i] ^ w.mask[(w.pos+i)&3]
		}
		w.pos += n
		p = p[n:]
		if w.scan.write(buf[:n]) {
			return true
		}
	}
	return false
}

// wsFrameTracker follows the server-to-client frames the proxy relays, to know whether the
// next byte it writes starts a frame. A close frame injected anywhere else would land inside a
// frame the client is still reading and corrupt it.
type wsFrameTracker struct {
	hdr  [14]byte
	nhdr int
	left uint64 // payload bytes left in the current frame
	lost bool   // an impossible length: the boundary is unknown from here on
}

func (t *wsFrameTracker) boundary() bool { return !t.lost && t.nhdr == 0 && t.left == 0 }

func (t *wsFrameTracker) feed(b []byte) {
	for len(b) > 0 && !t.lost {
		if t.left > 0 {
			n := len(b)
			if uint64(n) > t.left {
				n = int(t.left)
			}
			t.left -= uint64(n)
			b = b[n:]
			continue
		}
		t.hdr[t.nhdr] = b[0]
		t.nhdr++
		b = b[1:]
		if t.nhdr < 2 {
			continue
		}
		need, l := 2, t.hdr[1]&0x7f
		switch l {
		case 126:
			need = 4
		case 127:
			need = 10
		}
		if t.hdr[1]&0x80 != 0 { // server frames are not masked; tolerate it anyway
			need += 4
		}
		if t.nhdr < need {
			continue
		}
		switch l {
		case 126:
			t.left = uint64(binary.BigEndian.Uint16(t.hdr[2:4]))
		case 127:
			if t.left = binary.BigEndian.Uint64(t.hdr[2:10]); t.left>>63 != 0 {
				t.lost = true
			}
		default:
			t.left = uint64(l)
		}
		t.nhdr = 0
	}
}

// wsGuardConn is the hijacked client connection of a Realtime socket. Reads of a guarded
// socket pass through the inspector; a hit drops the data read and fails the read, which makes
// the reverse proxy tear both sides down. Writes are serialized, and a close frame with a reason
// is sent only when the last byte relayed to the client ended a frame; otherwise the connection
// is simply torn down.
type wsGuardConn struct {
	net.Conn
	r       io.Reader
	insp    *wsInspector // nil: the socket is only tracked, not inspected
	onBlock func(reason string)
	onClose func()

	wmu    sync.Mutex
	closed bool
	srv    wsFrameTracker
	err    error

	closeOnce sync.Once
}

const wsCloseReason = "legacy API keys are disabled for this project"

func (c *wsGuardConn) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	if n > 0 && c.insp != nil {
		if found, why := c.insp.feed(p[:n]); found || why != "" {
			if found {
				why = wsCloseReason
			}
			c.err = errLegacyKeyInStream
			c.block(why)
			return 0, c.err
		}
	}
	return n, err
}

func (c *wsGuardConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	n, err := c.Conn.Write(p)
	c.srv.feed(p[:n])
	return n, err
}

func (c *wsGuardConn) Close() error {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	return c.Conn.Close()
}

// sendClose stops further writes and, when the client's frame stream is at a frame boundary,
// sends a close frame (1008, policy violation) with reason.
func (c *wsGuardConn) sendClose(reason string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if !c.srv.boundary() {
		return
	}
	if len(reason) > 123 {
		reason = reason[:123]
	}
	frame := append([]byte{0x88, byte(2 + len(reason)), 0x03, 0xF0}, reason...)
	_ = c.Conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Conn.Write(frame)
}

// block ends the connection because the client sent a legacy key or broke a framing rule.
func (c *wsGuardConn) block(reason string) {
	c.sendClose(reason)
	if c.onBlock != nil {
		c.onBlock(reason)
	}
}

// sever ends the connection from outside (the project's keys changed); the blocked Read makes
// the reverse proxy tear down the upstream side.
func (c *wsGuardConn) sever(reason string) {
	c.sendClose(reason)
	_ = c.Close()
}

// socketSet tracks the Realtime sockets that were opened without inspection (the legacy keys
// were enabled), per project, so they can be closed when the project's legacy keys are
// switched off: nothing inspects them, and one can carry a legacy access_token for as long as
// the client keeps it open.
type socketSet struct {
	mu sync.Mutex
	m  map[string]map[*wsGuardConn]struct{}
}

func (s *socketSet) add(ref string, c *wsGuardConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]map[*wsGuardConn]struct{}{}
	}
	if s.m[ref] == nil {
		s.m[ref] = map[*wsGuardConn]struct{}{}
	}
	s.m[ref][c] = struct{}{}
}

func (s *socketSet) remove(ref string, c *wsGuardConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m[ref], c)
	if len(s.m[ref]) == 0 {
		delete(s.m, ref)
	}
}

// refs lists the projects with a tracked socket; with ref set, just that project if it has one.
func (s *socketSet) refs(ref string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref != "" {
		if len(s.m[ref]) > 0 {
			return []string{ref}
		}
		return nil
	}
	out := make([]string, 0, len(s.m))
	for r := range s.m {
		out = append(out, r)
	}
	return out
}

// closeAll severs every tracked socket of ref and reports how many there were.
func (s *socketSet) closeAll(ref, reason string) int {
	s.mu.Lock()
	conns := make([]*wsGuardConn, 0, len(s.m[ref]))
	for c := range s.m[ref] {
		conns = append(conns, c)
	}
	delete(s.m, ref)
	s.mu.Unlock()
	for _, c := range conns {
		c.sever(reason)
	}
	return len(conns)
}

// guardedWriter makes the hijacked connection of an upgrade go through wrap.
type guardedWriter struct {
	http.ResponseWriter
	wrap func(net.Conn, *bufio.ReadWriter) net.Conn
}

func (w *guardedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return w.wrap(c, brw), brw, nil
}

func (w *guardedWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *guardedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// isWebSocketHandshake reports whether r is a WebSocket opening handshake: a GET that asks
// for the "websocket" upgrade with a Connection header that lists "upgrade" (the condition
// under which httputil.ReverseProxy switches protocols).
func isWebSocketHandshake(r *http.Request) bool {
	return r.Method == http.MethodGet && connectionUpgrade(r.Header) && strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// connectionUpgrade reports whether a Connection header lists the "upgrade" token.
func connectionUpgrade(h http.Header) bool {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}
