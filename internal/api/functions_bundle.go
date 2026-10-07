package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	BundleFileName = ".supavise-bundle.ezbr"
	// BundleInfoFileName is stored next to a bundle that the node made from uploaded sources
	// (the sources are stored too, so they can be read and downloaded again); it holds the
	// module specifier of the entrypoint inside the bundle, see SourceBundleInfo.
	BundleInfoFileName = ".supavise-bundle.json"
	bundleMagic        = "EZBR"
)

// SourceBundleInfo is the content of the BundleInfoFileName file.
type SourceBundleInfo struct {
	// Entrypoint is the module specifier of the entrypoint inside the bundle.
	Entrypoint string `json:"entrypoint"`
}

// SourceBundle is an upload of source files that the node has to bundle.
type SourceBundle struct {
	Ref, Slug string
	// Files are the sources, paths relative to the project's working directory.
	Files []FunctionFile
	// Entrypoint and ImportMap are paths among Files (ImportMap may be empty).
	Entrypoint, ImportMap string
	// StaticPatterns are the patterns of files to embed.
	StaticPatterns []string
}

// BundledSource is the result of bundling: the upload as the CLI would have sent it.
type BundledSource struct {
	// Bundle is "EZBR" and the Brotli stream of the eszip.
	Bundle []byte
	// Entrypoint is the module specifier of the entrypoint inside the bundle.
	Entrypoint string
}

// SourceBundler is implemented by a FunctionsHook that can bundle uploaded sources in a
// sandbox (internal/functions). A node that runs Edge Functions never serves source files,
// so without one its API refuses source uploads.
type SourceBundler interface {
	BundleSources(ctx context.Context, in SourceBundle) (*BundledSource, error)
}

// BundleError is a failure of the uploaded code or files (a module that does not exist, a
// syntax error): the message is shown to the uploader.
type BundleError struct{ Msg string }

func (e *BundleError) Error() string { return e.Msg }

var (
	// ErrBundlingUnavailable means this node cannot bundle sources at all.
	ErrBundlingUnavailable = errors.New("this node cannot bundle uploaded sources")
	// ErrBundlingBusy means too many uploads are waiting to be bundled.
	ErrBundlingBusy = errors.New("the bundler is busy")
)

// bundleStoredFiles is what is stored for sources that were bundled: the sources, the
// bundle, and the entrypoint inside it.
func bundleStoredFiles(files []FunctionFile, b *BundledSource) []FunctionFile {
	info, _ := json.Marshal(SourceBundleInfo{Entrypoint: b.Entrypoint})
	out := append([]FunctionFile(nil), files...)
	return append(out, FunctionFile{Path: BundleFileName, Content: b.Bundle}, FunctionFile{Path: BundleInfoFileName, Content: info})
}

// storedSources returns the files of a function that the user uploaded, leaving out what the
// node added.
func storedSources(files []FunctionFile) []FunctionFile {
	var out []FunctionFile
	hasSources := false
	for _, f := range files {
		if f.Path != BundleFileName && f.Path != BundleInfoFileName {
			hasSources = true
		}
	}
	if !hasSources {
		return files // an uploaded bundle: the bundle is all there is
	}
	for _, f := range files {
		if f.Path != BundleFileName && f.Path != BundleInfoFileName {
			out = append(out, f)
		}
	}
	return out
}

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
