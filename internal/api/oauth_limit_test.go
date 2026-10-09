package api

import (
	"fmt"
	"testing"
	"time"
)

func TestWindowLimiterAllow(t *testing.T) {
	l := newWindowLimiter(3, time.Minute)
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3; i++ {
		if _, ok := l.allow("a", t0.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("event %d refused", i+1)
		}
	}
	retry, ok := l.allow("a", t0.Add(10*time.Second))
	if ok || retry != 50*time.Second {
		t.Errorf("fourth event: ok=%v retry=%v, want refused with 50s", ok, retry)
	}
	// A refused event is not counted: the window still ends a minute after the first event.
	if retry, ok := l.allow("a", t0.Add(59*time.Second)); ok || retry != time.Second {
		t.Errorf("at 59s: ok=%v retry=%v", ok, retry)
	}
	if _, ok := l.allow("a", t0.Add(time.Minute)); !ok {
		t.Error("a new window starts when the old one ends")
	}
	if _, ok := l.allow("b", t0.Add(10*time.Second)); !ok {
		t.Error("keys are independent")
	}
	// Retry is at least a second even at the very end of a window.
	l2 := newWindowLimiter(1, time.Minute)
	l2.allow("x", t0)
	if retry, ok := l2.allow("x", t0.Add(time.Minute-time.Millisecond)); ok || retry != time.Second {
		t.Errorf("end of window: ok=%v retry=%v", ok, retry)
	}
}

func TestWindowLimiterFailures(t *testing.T) {
	l := newWindowLimiter(2, time.Minute)
	t0 := time.Unix(1_800_000_000, 0)
	if _, blocked := l.blocked("a", t0); blocked {
		t.Fatal("blocked before any failure")
	}
	l.fail("a", t0)
	if _, blocked := l.blocked("a", t0); blocked {
		t.Fatal("blocked after one failure of two")
	}
	l.fail("a", t0.Add(time.Second))
	retry, blocked := l.blocked("a", t0.Add(2*time.Second))
	if !blocked || retry != 58*time.Second {
		t.Errorf("after two failures: blocked=%v retry=%v", blocked, retry)
	}
	// Asking does not count.
	for i := 0; i < 10; i++ {
		l.blocked("b", t0)
	}
	if _, blocked := l.blocked("b", t0); blocked {
		t.Error("blocked() counted")
	}
	if _, blocked := l.blocked("a", t0.Add(time.Minute)); blocked {
		t.Error("still blocked after the window")
	}
}

// The table is bounded. Windows that ended make room; when every slot is live, an address nobody
// has seen is not limited, and the ones that are stored still are.
func TestWindowLimiterBound(t *testing.T) {
	l := newWindowLimiter(1, time.Minute)
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < oauthLimiterMaxKeys; i++ {
		if _, ok := l.allow(fmt.Sprint("k", i), t0); !ok {
			t.Fatalf("key %d refused", i)
		}
	}
	if n := len(l.keys); n != oauthLimiterMaxKeys {
		t.Fatalf("%d keys", n)
	}
	// Full of live windows: a new key passes however often it asks, and is not stored.
	for i := 0; i < 5; i++ {
		if _, ok := l.allow("new", t0.Add(time.Second)); !ok {
			t.Fatal("an unknown address was limited while the table was full")
		}
	}
	if len(l.keys) != oauthLimiterMaxKeys {
		t.Errorf("the overflow was stored: %d keys", len(l.keys))
	}
	if _, ok := l.allow("k0", t0.Add(time.Second)); ok {
		t.Error("a stored address escaped its limit")
	}
	// Once the windows have ended they are dropped to make room.
	if _, ok := l.allow("new", t0.Add(2*time.Minute)); !ok {
		t.Fatal("refused after the windows ended")
	}
	if len(l.keys) != 1 {
		t.Errorf("%d keys after the windows ended, want 1", len(l.keys))
	}
	if _, ok := l.allow("new", t0.Add(2*time.Minute)); ok {
		t.Error("the stored key is not limited")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "1", time.Millisecond: "1", time.Second: "1", 1500 * time.Millisecond: "2", 10 * time.Minute: "600"} {
		if got := retryAfterSeconds(d); got != want {
			t.Errorf("retryAfterSeconds(%v) = %s, want %s", d, got, want)
		}
	}
}
