package backup

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

// newEncoder returns a zstd writer over w. conc is the number of compression goroutines.
func newEncoder(w io.Writer, conc int) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(conc),
		zstd.WithLowerEncoderMem(true),
	)
}

// newDecoder returns a zstd reader over r. Close must be called to release it.
func newDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
}

// countingWriter counts bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
