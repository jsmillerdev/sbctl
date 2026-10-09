package ctxutil

import (
	"context"
	"testing"
	"time"
)

func TestSleep(t *testing.T) {
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); err != context.Canceled || time.Since(start) > time.Second {
		t.Fatalf("cancelled sleep: %v after %v", err, time.Since(start))
	}
	if err := Sleep(ctx, 0); err != context.Canceled {
		t.Fatalf("zero sleep on a done context: %v", err)
	}
}
