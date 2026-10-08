package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/supavise/supavise/internal/replicas"
)

func infraPath(q url.Values) string {
	return "/platform/projects/" + testRef + "/infra-monitoring?" + q.Encode()
}

func lagQuery(id string, attrs ...string) url.Values {
	q := url.Values{
		"startDate": {"2026-10-08T10:00:00Z"}, "endDate": {"2026-10-08T11:00:00Z"}, "interval": {"5m"},
		"attributes": {strings.Join(attrs, ",")},
	}
	if id != "" {
		q.Set("databaseIdentifier", id)
	}
	return q
}

// A request for the replication lag of a replica is answered from the controller's samples: the
// mean of each interval, as strings, in the shape Studio reads for one attribute.
func TestInfraMonitoringServesTheLagOfAReplica(t *testing.T) {
	rf := newReplicaFixture(t)
	id := rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	at := func(min, sec int) time.Time { return time.Date(2026, 10, 8, 10, min, sec, 0, time.UTC) }
	rf.svc.lag[id] = []replicas.LagPoint{
		{At: at(2, 0), Seconds: 1}, {At: at(4, 0), Seconds: 2}, // 10:00 bucket
		{At: at(5, 0), Seconds: 0.5},                                    // 10:05
		{At: at(12, 0), Seconds: -1},                                    // unknown lag is skipped
		{At: at(31, 30), Seconds: 7},                                    // 10:30
		{At: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Seconds: 9}, // after the end
	}

	rec := rf.do("GET", infraPath(lagQuery(id, lagAttribute)), nil)
	if rec.Code != 200 || rec.Header().Get("X-Supavise-Stub") != "" {
		t.Fatalf("%d %s stub=%q", rec.Code, rec.Body, rec.Header().Get("X-Supavise-Stub"))
	}
	var got struct {
		Data []map[string]string
		// The metadata Studio's chart scales by.
		YAxisLimit   float64 `json:"yAxisLimit"`
		Format       string  `json:"format"`
		Total        float64 `json:"total"`
		TotalAverage float64 `json:"totalAverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	var starts, values []string
	for _, p := range got.Data {
		starts, values = append(starts, p["period_start"]), append(values, p[lagAttribute])
	}
	if strings.Join(starts, " ") != "2026-10-08T10:00:00.000Z 2026-10-08T10:05:00.000Z 2026-10-08T10:30:00.000Z" || strings.Join(values, " ") != "1.5 0.5 7" {
		t.Fatalf("points = %v %v", starts, values)
	}
	if got.Format != "s" || got.YAxisLimit != 7 || got.Total != 9 || got.TotalAverage != 3 {
		t.Fatalf("meta = %+v", got)
	}
	if len(rf.svc.lagSince) != 1 || !rf.svc.lagSince[0].Equal(time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lag asked since %v", rf.svc.lagSince)
	}

	// Several attributes: the multi-attribute format, values by attribute, and an error entry for
	// each metric Supavise does not collect.
	rec = rf.do("GET", infraPath(lagQuery(id, lagAttribute, "cpu_usage")), nil)
	var multi struct {
		Data []struct {
			PeriodStart string            `json:"period_start"`
			Values      map[string]string `json:"values"`
		}
		Series map[string]map[string]any
		Errors map[string]struct{ Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &multi); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v: %s", rec.Code, err, rec.Body)
	}
	if len(multi.Data) != 3 || multi.Data[0].Values[lagAttribute] != "1.5" || multi.Series[lagAttribute]["format"] != "s" || multi.Errors["cpu_usage"].Message == "" {
		t.Fatalf("multi = %+v", multi)
	}
	if _, bad := multi.Errors[lagAttribute]; bad {
		t.Fatalf("the lag is reported as an error: %+v", multi.Errors)
	}

	// attributes may be repeated, and the older single attribute works too.
	q := lagQuery(id)
	q.Add("attributes", lagAttribute)
	if rec := rf.do("GET", infraPath(q), nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"1.5"`) {
		t.Fatalf("repeated attributes: %d %s", rec.Code, rec.Body)
	}
	q = lagQuery(id)
	q.Set("attribute", lagAttribute)
	if rec := rf.do("GET", infraPath(q), nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"1.5"`) {
		t.Fatalf("attribute: %d %s", rec.Code, rec.Body)
	}
}

// The primary has no lag: the series is empty. A database of another project, or one that does
// not exist, is a 404.
func TestInfraMonitoringLagOfOtherDatabases(t *testing.T) {
	rf := newReplicaFixture(t)
	rf.addHealthyReplica(t, euNode, "eu-west-1", "abcdef")
	for _, id := range []string{"", testRef} {
		rec := rf.do("GET", infraPath(lagQuery(id, lagAttribute)), nil)
		var got struct{ Data []any }
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != 200 || got.Data == nil || len(got.Data) != 0 {
			t.Fatalf("primary %q: %d %s", id, rec.Code, rec.Body)
		}
	}
	for _, id := range []string{
		"bcdefghijklmnopqrstu-rr-eu-west-1-abcdef", // a replica of another project
		testRef + "-rr-eu-west-1-zzzzzz",           // this project's, but not a replica
		"nonsense",
	} {
		if rec := rf.do("GET", infraPath(lagQuery(id, lagAttribute)), nil); rec.Code != 404 {
			t.Fatalf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	for name, mod := range map[string]func(url.Values){
		"no dates":     func(q url.Values) { q.Del("startDate"); q.Del("endDate") },
		"reversed":     func(q url.Values) { q.Set("startDate", "2026-10-09T10:00:00Z") },
		"bad interval": func(q url.Values) { q.Set("interval", "7m") },
	} {
		q := lagQuery("", lagAttribute)
		mod(q)
		if rec := rf.do("GET", infraPath(q), nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// A request for no metric of ours is answered as the spec-derived stub answered it.
func TestInfraMonitoringOtherAttributesKeepTheStubAnswer(t *testing.T) {
	rf := newReplicaFixture(t)
	for _, attrs := range [][]string{{"cpu_usage"}, {"cpu_usage", "ram_usage"}, nil} {
		rec := rf.do("GET", infraPath(lagQuery("", attrs...)), nil)
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("X-Supavise-Stub") != "true" {
			t.Fatalf("%v: %d %q stub=%q", attrs, rec.Code, rec.Body, rec.Header().Get("X-Supavise-Stub"))
		}
	}
}
