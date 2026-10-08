package api

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Infrastructure metrics. Supavise collects one of the attributes Studio asks for, the
// replication lag of a read replica, from the replica controller's ring of one-minute samples
// (24 hours, kept in memory, empty after the leader restarts). A request that names no other
// metric than that one is answered here; any other attribute has no source and is answered as
// before, with an empty 200, which Studio draws as an empty chart.

func (s *Server) routesInfraMonitoring(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/infra-monitoring", s.infraMonitoring)
}

// lagAttribute is the replication lag in seconds, Studio's name for it.
const lagAttribute = "physical_replication_lag_physical_replication_lag_seconds"

// infraIntervals are the buckets of the query's interval parameter.
var infraIntervals = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "10m": 10 * time.Minute, "30m": 30 * time.Minute, "1h": time.Hour, "1d": 24 * time.Hour,
}

// infraAttributes are the attributes a request names: attributes (repeated or comma-separated)
// and the older single attribute.
func infraAttributes(r *http.Request) []string {
	var out []string
	q := r.URL.Query()
	for _, v := range append(q["attributes"], q["attribute"]...) {
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// infraSeriesMeta is what Studio's chart needs to scale and label a series.
func infraSeriesMeta(values []float64) map[string]any {
	var sum, top float64
	for _, v := range values {
		sum += v
		top = max(top, v)
	}
	avg := 0.0
	if len(values) > 0 {
		avg = sum / float64(len(values))
	}
	return map[string]any{"yAxisLimit": math.Max(1, math.Ceil(top)), "format": "s", "total": roundMilli(sum), "totalAverage": roundMilli(avg)}
}

func roundMilli(v float64) float64 { return math.Round(v*1000) / 1000 }

func (s *Server) infraMonitoring(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	attrs := infraAttributes(r)
	if !slices.Contains(attrs, lagAttribute) {
		// No metric of ours: the spec-derived stub's answer, an empty 200.
		w.Header().Set("X-Supavise-Stub", "true")
		w.WriteHeader(http.StatusOK)
		return nil
	}
	q := r.URL.Query()
	start, err1 := time.Parse(time.RFC3339, q.Get("startDate"))
	end, err2 := time.Parse(time.RFC3339, q.Get("endDate"))
	if err1 != nil || err2 != nil || end.Before(start) {
		return errf(http.StatusBadRequest, "startDate and endDate must be RFC 3339 timestamps, the start not after the end")
	}
	step := time.Hour
	if v := q.Get("interval"); v != "" {
		if step = infraIntervals[v]; step == 0 {
			return errf(http.StatusBadRequest, "interval must be one of 1m, 5m, 10m, 30m, 1h or 1d")
		}
	}
	// The primary has no replication lag: it has no series.
	var points []bucketPoint
	if id := q.Get("databaseIdentifier"); id != "" && id != p.Ref {
		_, ok, err := s.replicaOf(r.Context(), p.Ref, id)
		if err != nil {
			return err
		}
		if !ok || s.replicas == nil {
			return errf(http.StatusNotFound, "Database not found")
		}
		samples, err := s.replicas.Lag(r.Context(), id, start)
		if err != nil {
			return replicaServiceErr(err)
		}
		type acc struct {
			sum float64
			n   int
		}
		sums := map[time.Time]*acc{}
		for _, sm := range samples {
			if sm.At.Before(start) || sm.At.After(end) || sm.Seconds < 0 {
				continue
			}
			b := sm.At.UTC().Truncate(step)
			if sums[b] == nil {
				sums[b] = &acc{}
			}
			sums[b].sum += sm.Seconds
			sums[b].n++
		}
		for b, a := range sums {
			points = append(points, bucketPoint{b, roundMilli(a.sum / float64(a.n))})
		}
		slices.SortFunc(points, func(a, b bucketPoint) int { return a.at.Compare(b.at) })
	}
	values := make([]float64, len(points))
	for i, pt := range points {
		values[i] = pt.v
	}
	meta := infraSeriesMeta(values)
	data := make([]any, 0, len(points))
	if len(attrs) == 1 {
		// One attribute: the value sits on the point, with the series' metadata on the response.
		for _, pt := range points {
			data = append(data, map[string]any{"period_start": ts(pt.at), lagAttribute: strconv.FormatFloat(pt.v, 'f', -1, 64)})
		}
		meta["data"] = data
		writeJSON(w, http.StatusOK, meta)
		return nil
	}
	// Several attributes: values by attribute, one series entry each. The others have no source.
	for _, pt := range points {
		data = append(data, map[string]any{"period_start": ts(pt.at), "values": map[string]any{lagAttribute: strconv.FormatFloat(pt.v, 'f', -1, 64)}})
	}
	errs := map[string]any{}
	for _, a := range attrs {
		if a != lagAttribute {
			errs[a] = map[string]any{"message": "Supavise does not collect this metric"}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "series": map[string]any{lagAttribute: meta}, "errors": errs})
	return nil
}

// bucketPoint is the mean lag of one interval.
type bucketPoint struct {
	at time.Time
	v  float64
}
