package api

import (
	"net/http"
	"strings"
)

// regionNames are the display names Studio shows for the region codes it knows
// (packages/shared-data/regions.ts at the pinned Studio commit; config.Regions has the codes).
var regionNames = map[string]string{
	"us-west-1":      "West US (North California)",
	"us-west-2":      "West US (Oregon)",
	"us-east-1":      "East US (North Virginia)",
	"us-east-2":      "East US (Ohio)",
	"ca-central-1":   "Canada (Central)",
	"eu-west-1":      "West EU (Ireland)",
	"eu-west-2":      "West Europe (London)",
	"eu-west-3":      "West EU (Paris)",
	"eu-central-1":   "Central EU (Frankfurt)",
	"eu-central-2":   "Central Europe (Zurich)",
	"eu-north-1":     "North EU (Stockholm)",
	"ap-south-1":     "South Asia (Mumbai)",
	"ap-southeast-1": "Southeast Asia (Singapore)",
	"ap-northeast-1": "Northeast Asia (Tokyo)",
	"ap-northeast-2": "Northeast Asia (Seoul)",
	"ap-southeast-2": "Oceania (Sydney)",
	"sa-east-1":      "South America (São Paulo)",
}

// smartGroupOf is the smart region group a code belongs to, as hosted groups them.
func smartGroupOf(code string) (string, string) {
	switch {
	case strings.HasPrefix(code, "eu-"):
		return "emea", "EMEA"
	case strings.HasPrefix(code, "ap-"):
		return "apac", "APAC"
	}
	return "americas", "Americas"
}

func (s *Server) routesRegions(add func(string, handlerFunc)) {
	add("GET /platform/projects/available-regions", s.availableRegions)
	add("GET /v1/projects/available-regions", s.availableRegions)
}

// availableRegions answers Studio's new-project form. Every project of a node runs on the node,
// so the one region offered is the node's configured region (config region), which is also the
// recommendation; no smart groups are offered, so Studio shows the single specific region.
func (s *Server) availableRegions(w http.ResponseWriter, r *http.Request) error {
	code := s.cfg.ProjectRegion("")
	name := regionNames[code]
	if name == "" {
		name = code
	}
	provider := r.URL.Query().Get("cloud_provider")
	if provider != "AWS_K8S" && provider != "AWS_NIMBUS" {
		provider = "AWS"
	}
	region := map[string]any{"name": name, "code": code, "type": "specific", "provider": provider}
	groupCode, groupName := smartGroupOf(code)
	writeJSON(w, http.StatusOK, map[string]any{
		"recommendations": map[string]any{
			"smartGroup": map[string]any{"name": groupName, "code": groupCode, "type": "smartGroup"},
			"specific":   []any{region},
		},
		"all": map[string]any{
			"smartGroup": []any{},
			"specific":   []any{region},
		},
	})
	return nil
}
