package replicas

import "time"

const (
	// lagWindow is how long lag samples are kept (design 2.7.7).
	lagWindow = 24 * time.Hour
	lagStep   = time.Minute
	lagSlots  = int(lagWindow / lagStep)
)

// lagRing keeps one point per minute for the last 24 hours: the mean of the samples taken in
// that minute. It lives in memory, so a leader restart empties it. Not safe for concurrent use;
// the controller holds its lock around it.
type lagRing struct {
	slots [lagSlots]lagBucket
}

type lagBucket struct {
	minute int64 // Unix minutes; 0 means the slot was never written
	sum    float64
	n      int
}

func minuteOf(t time.Time) int64 { return t.Unix() / 60 }

// add records a sample taken at t.
func (r *lagRing) add(t time.Time, seconds float64) {
	m := minuteOf(t)
	b := &r.slots[int(m%int64(lagSlots))]
	if b.minute != m {
		*b = lagBucket{minute: m}
	}
	b.sum += seconds
	b.n++
}

// since returns the points from the minute of from up to the minute of now, oldest first.
func (r *lagRing) since(from, now time.Time) []LagPoint {
	last := minuteOf(now)
	first := max(minuteOf(from), last-int64(lagSlots)+1)
	var out []LagPoint
	for m := first; m <= last; m++ {
		b := r.slots[int(m%int64(lagSlots))]
		if b.minute == m && b.n > 0 {
			out = append(out, LagPoint{At: time.Unix(m*60, 0).UTC(), Seconds: b.sum / float64(b.n)})
		}
	}
	return out
}
