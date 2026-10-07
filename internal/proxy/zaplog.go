package proxy

import (
	"context"
	"log/slog"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// zapToSlog returns a zap logger that writes to l, so CertMagic's certificate
// operations show up in the same journald stream as the rest of sbctl.
func zapToSlog(l *slog.Logger) *zap.Logger { return zap.New(&slogCore{l: l}) }

type slogCore struct {
	l      *slog.Logger
	fields []zapcore.Field
}

func slogLevelOf(l zapcore.Level) slog.Level {
	switch {
	case l >= zapcore.ErrorLevel:
		return slog.LevelError
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	}
	return slog.LevelDebug
}

func (c *slogCore) Enabled(l zapcore.Level) bool {
	return c.l.Enabled(context.Background(), slogLevelOf(l))
}

func (c *slogCore) With(fs []zapcore.Field) zapcore.Core {
	return &slogCore{l: c.l, fields: append(append([]zapcore.Field(nil), c.fields...), fs...)}
}

func (c *slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *slogCore) Write(e zapcore.Entry, fs []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fs {
		f.AddTo(enc)
	}
	attrs := make([]any, 0, 2*len(enc.Fields)+2)
	if e.LoggerName != "" {
		attrs = append(attrs, "logger", e.LoggerName)
	}
	for k, v := range enc.Fields {
		attrs = append(attrs, k, v)
	}
	c.l.Log(context.Background(), slogLevelOf(e.Level), "certmagic: "+e.Message, attrs...)
	return nil
}

func (c *slogCore) Sync() error { return nil }
