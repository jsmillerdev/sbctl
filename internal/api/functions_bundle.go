package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"strings"
)

const (
	// EszipMediaType is the Content-Type of a bundled function: the Supabase CLI bundles
	// the function itself (natively or with Docker) and uploads the result with POST (create)
	// or PATCH (update) on /v1/projects/{ref}/functions[/{slug}].
	EszipMediaType = "application/vnd.denoland.eszip"
	// BundleFileName is the stored name of an uploaded bundle among a function's files. The
	// body is kept as the CLI sent it: the bytes "EZBR" and a Brotli-compressed eszip.
	BundleFileName = ".sbctl-bundle.ezbr"
	bundleMagic    = "EZBR"
)

// isBundleUpload reports whether the request carries a bundled function.
func isBundleUpload(r *http.Request) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return ct == EszipMediaType
}

// readBundle reads and checks an uploaded bundle and returns it as the function's only
// file. The query parameter ezbr_sha256, when the client sends it, is the SHA-256 of the
// body.
func readBundle(r *http.Request) ([]FunctionFile, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeploy+1))
	if err != nil {
		return nil, errf(http.StatusBadRequest, "Could not read the body: %v", err)
	}
	if len(body) > maxDeploy {
		return nil, errf(http.StatusRequestEntityTooLarge, "Function upload too large")
	}
	if !bytes.HasPrefix(body, []byte(bundleMagic)) {
		return nil, errf(http.StatusBadRequest, "The body is not a compressed eszip bundle (it does not start with %s)", bundleMagic)
	}
	if want := strings.ToLower(r.URL.Query().Get("ezbr_sha256")); want != "" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != want {
			return nil, errf(http.StatusBadRequest, "ezbr_sha256 does not match the body")
		}
	}
	return []FunctionFile{{Path: BundleFileName, Content: body}}, nil
}

// queryVerifyJWT reads the optional verify_jwt parameter of a bundle upload.
func queryVerifyJWT(r *http.Request) *bool {
	v := r.URL.Query().Get("verify_jwt")
	if v == "" {
		return nil
	}
	b := v != "false"
	return &b
}

// updateFunctionBundle is PATCH with a bundle body: the CLI's redeploy of a function it
// bundled itself. The metadata comes from the query, as on create.
func (s *Server) updateFunctionBundle(w http.ResponseWriter, r *http.Request, ref string, f *Function) error {
	q := r.URL.Query()
	if v := queryVerifyJWT(r); v != nil {
		f.VerifyJWT = *v
	}
	if v := q.Get("entrypoint_path"); v != "" {
		f.EntrypointPath = v
	}
	if q.Has("import_map_path") {
		f.ImportMapPath = q.Get("import_map_path")
	}
	if v := q.Get("name"); v != "" {
		f.Name = v
	}
	files, err := readBundle(r)
	if err != nil {
		return err
	}
	f.Ref = ref
	if err := s.store.UpsertFunction(r.Context(), f, files); err != nil {
		return err
	}
	if err := s.functionsChanged(r.Context(), ref); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, fnJSON(f))
	return nil
}
