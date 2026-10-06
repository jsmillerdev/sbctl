package main

import (
	"net/http"
	"strings"
)

// zeroValue is the neutral JSON value for a field (see field).
func zeroValue(z string) any {
	switch {
	case z == "s":
		return ""
	case z == "i":
		return 0
	case z == "b":
		return false
	case z == "a":
		return []any{}
	case z == "o":
		return map[string]any{}
	case strings.HasPrefix(z, "'"):
		return z[1:]
	}
	return nil
}

// stubBody builds the empty answer for a documented shape: an empty array, an object holding
// the required fields with neutral values, or nothing.
func stubBody(sh *shape) any {
	switch sh.Kind {
	case "array":
		return []any{}
	case "object":
		m := map[string]any{}
		for _, f := range sh.Required {
			m[f.Name] = zeroValue(f.Zero)
		}
		return m
	}
	return nil
}

// stubHandler answers a documented route with the empty value of its documented shape and the
// documented success status. The research table calls these "defaults" or "yes" rows.
func stubHandler(w *respWriter, r *http.Request, c *reqCtx) {
	rt, _ := r.Context().Value(routeKey{}).(*route)
	if rt == nil || rt.shape == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body := stubBody(rt.shape)
	if body == nil {
		w.WriteHeader(rt.shape.Status)
		return
	}
	w.json(rt.shape.Status, body)
}
