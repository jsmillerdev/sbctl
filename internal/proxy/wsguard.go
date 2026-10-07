package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/OWNER/sbctl/internal/secrets"
)

// With the legacy keys disabled, the anon and service_role JWTs must stop working on
// Realtime too. Realtime also takes a JWT inside the socket (the access_token of a phx_join
// payload or of an "access_token" event) and authorizes channels with its claims, which the
// handshake check cannot see. So the proxy watches the client-to-server direction of a
// /realtime/v1 socket, and the body of a long-poll request, for one of the project's exact
// legacy keys, and ends the connection when it finds one. Everything else streams untouched.
//
// Memory per connection is constant: a frame is never buffered. Its payload is unmasked in a
// small scratch buffer, JSON string escapes are decoded on the fly (so "eyJ..." is the
// same key as "eyJ..."), and a window of the last len(key)-1 decoded bytes is kept so a key
// split across reads, frames or message fragments is still found.

var errLegacyKeyInStream = errors.New("proxy: a legacy API key was sent while the project's legacy keys are disabled")

// legacyNeedles returns the exact keys to look for, or nil when the legacy keys are enabled.
func legacyNeedles(k *secrets.ProjectKeys) [][]byte {
	if k == nil || !k.LegacyDisabled {
		return nil
	}
	var n [][]byte
	for _, key := range []string{k.AnonKey, k.ServiceRoleKey} {
		if key != "" {
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

// wsGuardConn is the hijacked client connection of a guarded socket. Reads pass through the
// inspector; a hit sends a close frame with a reason, drops the data read and fails the read,
// which makes the reverse proxy tear both sides down. Writes are serialized so the close frame
// cannot land inside another frame the proxy is relaying to the client.
type wsGuardConn struct {
	net.Conn
	r       io.Reader
	insp    *wsInspector
	onBlock func(reason string)

	wmu    sync.Mutex
	closed bool
	err    error
}

const wsCloseReason = "legacy API keys are disabled for this project"

func (c *wsGuardConn) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.r.Read(p)
	if n > 0 {
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
	return c.Conn.Write(p)
}

// block sends a close frame (1008, policy violation) and stops further writes.
func (c *wsGuardConn) block(reason string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if !c.closed {
		c.closed = true
		if len(reason) > 123 {
			reason = reason[:123]
		}
		frame := append([]byte{0x88, byte(2 + len(reason)), 0x03, 0xF0}, reason...)
		_ = c.Conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Conn.Write(frame)
	}
	if c.onBlock != nil {
		c.onBlock(reason)
	}
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

// maxGuardedBody bounds the memory held for a long-poll request body. A long-poll body is one
// or a few Phoenix messages; it is read in full, so that a refused one never reaches Realtime
// partly (a streamed check would have sent everything before the key upstream already).
const maxGuardedBody = 4 << 20

// checkBody reads the request body of a guarded non-upgrade request, answers the refusal itself
// when it carries a legacy key or is too large, and otherwise puts the body back for forwarding.
func checkBody(w http.ResponseWriter, r *http.Request, needles [][]byte) (ok bool, hit bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return true, false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGuardedBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, "Request body too large")
		} else {
			writeJSON(w, http.StatusBadRequest, "Bad Request")
		}
		return false, false
	}
	if newKeyScanner(needles).write(b) {
		writeText(w, http.StatusUnauthorized, msgInvalidKey)
		return false, true
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return true, false
}
