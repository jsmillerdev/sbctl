package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"regexp"
	"strings"

	"github.com/OWNER/sbctl/internal/registry"
)

// maxDeploy bounds one function upload (all files together).
const maxDeploy = 64 << 20

var slugRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

func (s *Server) routesFunctions(add func(string, handlerFunc)) {
	add("GET /v1/projects/{ref}/functions", s.listFunctions)
	add("POST /v1/projects/{ref}/functions", s.createFunction)
	add("POST /v1/projects/{ref}/functions/deploy", s.deployFunction)
	add("GET /v1/projects/{ref}/functions/{function_slug}", s.getFunction)
	add("PATCH /v1/projects/{ref}/functions/{function_slug}", s.updateFunction)
	add("DELETE /v1/projects/{ref}/functions/{function_slug}", s.deleteFunction)
	add("GET /v1/projects/{ref}/functions/{function_slug}/body", s.functionBody)
}

func fnStatus(f *Function) string {
	if f.Status == "" {
		return "ACTIVE"
	}
	return f.Status
}

// fnJSON is the function as the list/get/deploy responses carry it. The three
// schemas differ only in their enum types, so one map serves them all.
func fnJSON(f *Function) map[string]any {
	m := map[string]any{
		"id": f.ID, "slug": f.Slug, "name": f.Name, "status": fnStatus(f), "version": f.Version,
		"created_at": f.CreatedAt.UnixMilli(), "updated_at": f.UpdatedAt.UnixMilli(), "verify_jwt": f.VerifyJWT,
	}
	if f.EntrypointPath != "" {
		m["entrypoint_path"] = f.EntrypointPath
	}
	if f.ImportMapPath != "" {
		m["import_map_path"] = f.ImportMapPath
		m["import_map"] = true
	}
	return m
}

func (s *Server) listFunctions(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	fns, err := s.store.ListFunctions(r.Context(), p.Ref)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(fns))
	for i := range fns {
		out = append(out, fnJSON(&fns[i]))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) loadFunction(r *http.Request) (*registry.Project, *Function, error) {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return nil, nil, err
	}
	f, err := s.store.GetFunction(r.Context(), p.Ref, r.PathValue("function_slug"))
	if errors.Is(err, ErrNotFound) {
		return nil, nil, errf(http.StatusNotFound, "Function not found")
	}
	return p, f, err
}

func (s *Server) getFunction(w http.ResponseWriter, r *http.Request) error {
	_, f, err := s.loadFunction(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, fnJSON(f))
	return nil
}

func (s *Server) deleteFunction(w http.ResponseWriter, r *http.Request) error {
	p, f, err := s.loadFunction(r)
	if err != nil {
		return err
	}
	if err := s.store.DeleteFunction(r.Context(), p.Ref, f.Slug); err != nil {
		return mapErr(err)
	}
	if err := s.functionsChanged(r.Context(), p.Ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// updateFunction changes metadata of a function (PATCH). A new body is deployed
// through the deploy route.
func (s *Server) updateFunction(w http.ResponseWriter, r *http.Request) error {
	p, f, err := s.loadFunction(r)
	if err != nil {
		return err
	}
	if isBundleUpload(r) {
		return s.updateFunctionBundle(w, r, p.Ref, f)
	}
	var in struct {
		Name       *string `json:"name"`
		VerifyJWT  *bool   `json:"verify_jwt"`
		Entrypoint *string `json:"entrypoint_path"`
		ImportMap  *string `json:"import_map_path"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if s.fnHook != nil && (in.Entrypoint != nil && *in.Entrypoint != f.EntrypointPath || in.ImportMap != nil && *in.ImportMap != f.ImportMapPath) {
		// What the runtime serves is the bundle made at deploy time, which a change of these
		// two paths would not touch: refuse instead of recording something that is not true.
		if files, err := s.store.FunctionFiles(r.Context(), p.Ref, f.Slug); err == nil && len(storedSources(files)) != len(files) {
			return errf(http.StatusBadRequest, "The entrypoint and the import map of a function are fixed when it is deployed, because this node bundles the uploaded sources then. Deploy the function again to change them.")
		}
	}
	if in.Name != nil {
		f.Name = *in.Name
	}
	if in.VerifyJWT != nil {
		f.VerifyJWT = *in.VerifyJWT
	}
	if in.Entrypoint != nil {
		f.EntrypointPath = *in.Entrypoint
	}
	if in.ImportMap != nil {
		f.ImportMapPath = *in.ImportMap
	}
	f.Ref = p.Ref
	if err := s.store.UpsertFunction(r.Context(), f, nil); err != nil {
		return err
	}
	if err := s.functionsChanged(r.Context(), p.Ref); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, fnJSON(f))
	return nil
}

// createFunction is the legacy JSON create (no body upload).
func (s *Server) createFunction(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	var in struct {
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		VerifyJWT  *bool  `json:"verify_jwt"`
		Entrypoint string `json:"entrypoint_path"`
		ImportMap  string `json:"import_map_path"`
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" || ct == "" {
		if err := decode(r, &in); err != nil {
			return err
		}
	} else {
		in.Slug = r.URL.Query().Get("slug")
		in.Name = r.URL.Query().Get("name")
		in.Entrypoint = r.URL.Query().Get("entrypoint_path")
		in.ImportMap = r.URL.Query().Get("import_map_path")
		in.VerifyJWT = queryVerifyJWT(r)
	}
	if !slugRe.MatchString(in.Slug) {
		return errf(http.StatusBadRequest, "Invalid function slug")
	}
	f := &Function{Ref: p.Ref, Slug: in.Slug, Name: firstNonEmpty(in.Name, in.Slug), Status: "ACTIVE", VerifyJWT: in.VerifyJWT == nil || *in.VerifyJWT,
		EntrypointPath: in.Entrypoint, ImportMapPath: in.ImportMap}
	var files []FunctionFile
	if isBundleUpload(r) {
		if in.Entrypoint == "" {
			return errf(http.StatusBadRequest, "entrypoint_path is required for a bundle")
		}
		if files, err = readBundle(r); err != nil {
			return err
		}
	}
	if err := s.store.UpsertFunction(r.Context(), f, files); err != nil {
		return err
	}
	if err := s.functionsChanged(r.Context(), p.Ref); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, fnJSON(f))
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// cleanFilePath makes a client file name safe to store: relative, no "..".
func cleanFilePath(name string) (string, bool) {
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "", false
		}
	}
	n := path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	n = strings.TrimPrefix(n, "/")
	return n, n != "" && n != "." && !strings.Contains(n, "..")
}

// deployFunction stores a multipart upload: a "metadata" JSON field and one "file"
// part per source file (file name = path inside the function).
func (s *Server) deployFunction(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return errf(http.StatusBadRequest, "Expected multipart/form-data")
	}
	var meta struct {
		EntrypointPath string `json:"entrypoint_path"`
		ImportMapPath  string `json:"import_map_path"`
		VerifyJWT      *bool  `json:"verify_jwt"`
		Name           string `json:"name"`
		// StaticPatterns are the files to embed in the bundle (config.toml static_files).
		StaticPatterns []string `json:"static_patterns"`
	}
	var files []FunctionFile
	var total int64
	haveMeta := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errf(http.StatusBadRequest, "Invalid multipart body: %v", err)
		}
		data, err := io.ReadAll(io.LimitReader(part, maxDeploy+1))
		if err != nil {
			return errf(http.StatusBadRequest, "Invalid multipart body: %v", err)
		}
		if total += int64(len(data)); total > maxDeploy {
			return errf(http.StatusRequestEntityTooLarge, "Function upload too large")
		}
		switch {
		case part.FormName() == "metadata":
			if err := json.Unmarshal(data, &meta); err != nil {
				return errf(http.StatusBadRequest, "Invalid metadata: %v", err)
			}
			haveMeta = true
		case partFileName(part) != "":
			// Part.FileName() keeps only the base name; function files keep their
			// directories (imports are relative).
			raw := partFileName(part)
			name, ok := cleanFilePath(raw)
			if !ok {
				return errf(http.StatusBadRequest, "Invalid file name %q", raw)
			}
			if name == BundleFileName || name == BundleInfoFileName {
				return errf(http.StatusBadRequest, "The file name %q is reserved", raw)
			}
			files = append(files, FunctionFile{Path: name, Content: data})
		}
	}
	if !haveMeta || meta.EntrypointPath == "" {
		return errf(http.StatusBadRequest, "metadata.entrypoint_path is required")
	}
	if len(files) == 0 {
		return errf(http.StatusBadRequest, "At least one file is required")
	}
	slug := firstNonEmpty(r.URL.Query().Get("slug"), slugFromName(meta.Name))
	if !slugRe.MatchString(slug) {
		return errf(http.StatusBadRequest, "Invalid function slug")
	}
	entry, _ := cleanFilePath(meta.EntrypointPath)
	f := &Function{Ref: p.Ref, Slug: slug, Name: firstNonEmpty(meta.Name, slug), Status: "ACTIVE",
		VerifyJWT: meta.VerifyJWT == nil || *meta.VerifyJWT, EntrypointPath: entry}
	if meta.ImportMapPath != "" {
		f.ImportMapPath, _ = cleanFilePath(meta.ImportMapPath)
	}
	if s.fnHook != nil {
		// A node that runs Edge Functions serves bundles only (see internal/functions: a
		// function run from source files could import other projects' files), so the sources
		// are bundled in a sandbox first and stored together with the bundle.
		if !hasFile(files, entry) {
			return errf(http.StatusBadRequest, "The entrypoint %q is not among the uploaded files", entry)
		}
		var static []string
		for _, sp := range meta.StaticPatterns {
			if clean, ok := cleanFilePath(sp); ok {
				static = append(static, clean)
			}
		}
		bundled, err := s.bundleSources(r.Context(), SourceBundle{Ref: p.Ref, Slug: slug, Files: files,
			Entrypoint: entry, ImportMap: f.ImportMapPath, StaticPatterns: static})
		if err != nil {
			return err
		}
		files = bundleStoredFiles(files, bundled)
	}
	if err := s.store.UpsertFunction(r.Context(), f, files); err != nil {
		return err
	}
	if err := s.functionsChanged(r.Context(), p.Ref); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, fnJSON(f))
	return nil
}

func hasFile(files []FunctionFile, p string) bool {
	for _, f := range files {
		if f.Path == p {
			return true
		}
	}
	return false
}

// bundleSources has the node's hook bundle an upload of sources and turns its failures into
// the API's answers.
func (s *Server) bundleSources(ctx context.Context, in SourceBundle) (*BundledSource, error) {
	const hint = " Deploy a bundle instead: run `supabase functions deploy` with Docker running, which bundles the function on your machine and uploads the bundle."
	b, ok := s.fnHook.(SourceBundler)
	if !ok {
		return nil, errf(http.StatusNotImplemented, "This node cannot bundle uploaded sources."+hint)
	}
	res, err := b.BundleSources(ctx, in)
	var be *BundleError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &be):
		return nil, errf(http.StatusBadRequest, "Could not bundle the function: %s", be.Msg)
	case errors.Is(err, ErrBundlingUnavailable):
		return nil, errf(http.StatusNotImplemented, "%v."+hint, err)
	case errors.Is(err, ErrBundlingBusy):
		return nil, errf(http.StatusTooManyRequests, "%v; try again in a minute", err)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	s.log.Error("edge functions: bundling uploaded sources", "ref", in.Ref, "slug", in.Slug, "err", err)
	return nil, errf(http.StatusInternalServerError, "Bundling failed on this node: %v", err)
}

// partFileName returns the filename parameter of a part's Content-Disposition as sent.
func partFileName(p *multipart.Part) string {
	_, params, err := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

func slugFromName(name string) string {
	return strings.Trim(regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(name, "-"), "-")
}

// functionBody returns the stored source files as multipart/form-data, the format
// `supabase functions download` reads.
func (s *Server) functionBody(w http.ResponseWriter, r *http.Request) error {
	p, f, err := s.loadFunction(r)
	if err != nil {
		return err
	}
	files, err := s.store.FunctionFiles(r.Context(), p.Ref, f.Slug)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, file := range storedSources(files) {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": file.Path}))
		h.Set("Content-Type", "application/octet-stream")
		part, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		if _, err := part.Write(file.Content); err != nil {
			return err
		}
	}
	if err := mw.Close(); err != nil {
		return err
	}
	writeRaw(w, http.StatusOK, mw.FormDataContentType(), buf.Bytes())
	return nil
}
