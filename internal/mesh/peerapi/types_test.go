package peerapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The path helpers fill in the wildcards of the patterns a server registers.
func TestPathsMatchTheirPatterns(t *testing.T) {
	const id = "abcdefghijklmnopqrst-rr-us-east-1-k3j9d2"
	type call struct{ method, path, pattern string }
	var gotValues []map[string]string
	var gotPatterns []string
	mux := http.NewServeMux()
	for _, c := range []call{
		{"GET", InstancePath(id), PathInstance},
		{"POST", InstanceActionPath(id, ActionPromote), PathInstanceAction},
		{"POST", PlanePath("system", PlaneStartDatabase), PathPlane},
		{"POST", BackupPath("system", BackupBase), PathBackup},
		{"POST", FleetRefreshPath(id), PathFleetRefresh},
	} {
		mux.HandleFunc(c.method+" "+c.pattern, func(w http.ResponseWriter, r *http.Request) {
			gotPatterns = append(gotPatterns, r.Pattern)
			vals := map[string]string{}
			for _, k := range []string{"identifier", "action", "ref", "method", "op", "tenant"} {
				if v := r.PathValue(k); v != "" {
					vals[k] = v
				}
			}
			gotValues = append(gotValues, vals)
		})
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
	}
	want := []map[string]string{
		{"identifier": id},
		{"identifier": id, "action": "promote"},
		{"ref": "system", "method": "start_database"},
		{"ref": "system", "op": "base"},
		{"tenant": id},
	}
	if len(gotValues) != len(want) {
		t.Fatalf("%d handler calls (%q), want %d", len(gotValues), gotPatterns, len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(gotValues[i], want[i]) {
			t.Errorf("%s: path values %v, want %v", gotPatterns[i], gotValues[i], want[i])
		}
	}
}

func TestPlaneMethodsAreDistinct(t *testing.T) {
	seen := map[PlaneMethod]bool{}
	for _, m := range PlaneMethods {
		if m == "" || seen[m] {
			t.Errorf("method %q is empty or listed twice", m)
		}
		seen[m] = true
	}
	if len(PlaneMethods) != 10 {
		t.Errorf("%d plane methods", len(PlaneMethods))
	}
}

func TestInstanceStatusJSON(t *testing.T) {
	in := InstanceStatus{Identifier: "x", Ref: "system", Role: "replica", Step: "5_replayed_wal_archives", At: time.Unix(1700000000, 0).UTC()}
	b, _ := json.Marshal(in)
	if strings.Contains(string(b), "lag_seconds") {
		t.Errorf("unknown lag is written: %s", b)
	}
	zero := 0.0
	in.LagSeconds = &zero
	b, _ = json.Marshal(in)
	if !strings.Contains(string(b), `"lag_seconds":0`) {
		t.Errorf("a lag of zero is not written: %s", b)
	}
	var out InstanceStatus
	if err := json.Unmarshal(b, &out); err != nil || out.LagSeconds == nil || *out.LagSeconds != 0 || out.Step != in.Step {
		t.Errorf("round trip: %+v, %v", out, err)
	}
}
