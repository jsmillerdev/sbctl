package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/OWNER/sbctl/internal/registry"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (s *Server) routesContent(add func(string, handlerFunc)) {
	add("GET /platform/projects/{ref}/content", s.listContent)
	add("PUT /platform/projects/{ref}/content", s.putContent)
	add("DELETE /platform/projects/{ref}/content", s.deleteContent)
	add("GET /platform/projects/{ref}/content/count", s.countContent)
	add("GET /platform/projects/{ref}/content/item/{id}", s.getContent)
	add("GET /platform/projects/{ref}/content/folders", s.listFolders)
	add("POST /platform/projects/{ref}/content/folders", s.createFolder)
	add("DELETE /platform/projects/{ref}/content/folders", s.deleteFolders)
	add("GET /platform/projects/{ref}/content/folders/{id}", s.listFolders)
	add("PATCH /platform/projects/{ref}/content/folders/{id}", s.renameFolder)
}

// contentJSON is a saved item in the shape of the content schemas; withBody and
// withOwner add the fields only the flat list and item routes carry.
func (s *Server) contentJSON(r *http.Request, p *registry.Project, c *Content, withBody, withOwner bool) (map[string]any, error) {
	m := map[string]any{
		"id": c.ID, "inserted_at": ts(c.InsertedAt), "updated_at": ts(c.UpdatedAt), "type": c.Type, "visibility": c.Visibility,
		"name": c.Name, "project_id": projectNumID(p), "owner_id": c.OwnerID, "favorite": c.Favorite,
		"last_updated_by": c.OwnerID,
	}
	if c.Description != "" {
		m["description"] = c.Description
	} else {
		m["description"] = nil
	}
	if c.FolderID != nil {
		m["folder_id"] = *c.FolderID
	} else {
		m["folder_id"] = nil
	}
	if withBody {
		var body any
		if len(c.Body) > 0 && json.Unmarshal(c.Body, &body) == nil {
			m["content"] = body
		} else {
			m["content"] = map[string]any{}
		}
	}
	if withOwner {
		name := ""
		if u, err := s.store.GetUserByID(r.Context(), c.OwnerID); err == nil {
			name = u.Username
		}
		m["owner"] = map[string]any{"id": c.OwnerID, "username": name}
		m["updated_by"] = map[string]any{"id": c.OwnerID, "username": name}
	}
	return m, nil
}

// visible reports whether u may see c: private items belong to their owner.
func visible(c *Content, u *User) bool { return c.Visibility != "user" || c.OwnerID == u.ID }

func (s *Server) listContent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	items, err := s.store.ListContent(r.Context(), p.Ref, ContentQuery{
		Type: q.Get("type"), Visibility: q.Get("visibility"), Favorite: q.Get("favorite") == "true", Name: q.Get("name"),
	})
	if err != nil {
		return err
	}
	limit := queryInt(r, "limit", 0)
	out := make([]map[string]any, 0, len(items))
	for i := range items {
		c := &items[i]
		if !visible(c, u) || (q.Get("visibility") == "user" && c.OwnerID != u.ID) {
			continue
		}
		m, err := s.contentJSON(r, p, c, true, true)
		if err != nil {
			return err
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if !uuidRe.MatchString(id) {
		return errf(http.StatusNotFound, "Content not found")
	}
	c, err := s.store.GetContent(r.Context(), p.Ref, id)
	if errors.Is(err, ErrNotFound) || (err == nil && !visible(c, u)) {
		return errf(http.StatusNotFound, "Content not found")
	}
	if err != nil {
		return err
	}
	m, err := s.contentJSON(r, p, c, true, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, m)
	return nil
}

// putContent creates or replaces a saved item (upsert by id). Fields the client
// leaves out keep their stored value.
func (s *Server) putContent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	in, err := jsonBody(r)
	if err != nil {
		return err
	}
	c := &Content{Ref: p.Ref, OwnerID: u.ID, Type: "sql", Visibility: "user"}
	if id := str(in, "id"); id != "" {
		if !uuidRe.MatchString(id) {
			return errf(http.StatusBadRequest, "id must be a uuid")
		}
		cur, err := s.store.GetContent(r.Context(), p.Ref, id)
		switch {
		case err == nil:
			if !visible(cur, u) {
				return errf(http.StatusNotFound, "Content not found")
			}
			*c = *cur
		case !errors.Is(err, ErrNotFound):
			return err
		}
		c.ID = id
	}
	if v, ok := in["name"].(string); ok {
		c.Name = v
	}
	if v, ok := in["description"].(string); ok {
		c.Description = v
	}
	if v, ok := in["type"].(string); ok {
		c.Type = v
	}
	if v, ok := in["visibility"].(string); ok {
		c.Visibility = v
	}
	if v, ok := in["favorite"].(bool); ok {
		c.Favorite = v
	}
	if v, ok := in["folder_id"]; ok {
		if f, _ := v.(string); f != "" && uuidRe.MatchString(f) {
			c.FolderID = &f
		} else {
			c.FolderID = nil
		}
	}
	if v, ok := in["content"]; ok {
		c.Body = mustJSON(v)
	}
	if strings.TrimSpace(c.Name) == "" {
		return errf(http.StatusBadRequest, "name is required")
	}
	if err := s.store.UpsertContent(r.Context(), c); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func idsParam(r *http.Request) []string {
	var ids []string
	for _, v := range r.URL.Query()["ids"] {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); uuidRe.MatchString(id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (s *Server) deleteContent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	deleted, err := s.store.DeleteContent(r.Context(), p.Ref, idsParam(r))
	if err != nil {
		return err
	}
	out := make([]map[string]string, 0, len(deleted))
	for _, id := range deleted {
		out = append(out, map[string]string{"id": id})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) countContent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	n, err := s.store.CountContent(r.Context(), p.Ref, u.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]int{"private": n.Private, "shared": n.Shared, "favorites": n.Favorites})
	return nil
}

// listFolders answers both the root listing (no id) and a folder's children.
func (s *Server) listFolders(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	var parent *string
	cq := ContentQuery{Type: r.URL.Query().Get("type"), Visibility: r.URL.Query().Get("visibility"), Name: r.URL.Query().Get("name")}
	if id := r.PathValue("id"); id != "" {
		if !uuidRe.MatchString(id) {
			return errf(http.StatusNotFound, "Folder not found")
		}
		parent = &id
		cq.FolderID = &id
	} else {
		cq.RootOnly = true
	}
	folders, err := s.store.ListFolders(r.Context(), p.Ref, parent)
	if err != nil {
		return err
	}
	items, err := s.store.ListContent(r.Context(), p.Ref, cq)
	if err != nil {
		return err
	}
	fs := make([]map[string]any, 0, len(folders))
	for _, f := range folders {
		if f.OwnerID != u.ID {
			continue
		}
		fs = append(fs, folderJSON(p, &f))
	}
	cs := make([]map[string]any, 0, len(items))
	for i := range items {
		if !visible(&items[i], u) {
			continue
		}
		m, err := s.contentJSON(r, p, &items[i], false, false)
		if err != nil {
			return err
		}
		cs = append(cs, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"folders": fs, "contents": cs}})
	return nil
}

func folderJSON(p *registry.Project, f *ContentFolder) map[string]any {
	m := map[string]any{"id": f.ID, "name": f.Name, "project_id": projectNumID(p), "owner_id": f.OwnerID, "parent_id": nil}
	if f.ParentID != nil {
		m["parent_id"] = *f.ParentID
	}
	return m
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	u, err := s.currentUser(r)
	if err != nil {
		return err
	}
	var in struct {
		Name     string `json:"name"`
		ParentID string `json:"parent_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Name) == "" {
		return errf(http.StatusBadRequest, "name is required")
	}
	f := &ContentFolder{Ref: p.Ref, OwnerID: u.ID, Name: in.Name}
	if in.ParentID != "" && uuidRe.MatchString(in.ParentID) {
		f.ParentID = &in.ParentID
	}
	if err := s.store.CreateFolder(r.Context(), f); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, folderJSON(p, f))
	return nil
}

func (s *Server) renameFolder(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	if !uuidRe.MatchString(id) || strings.TrimSpace(in.Name) == "" {
		return errf(http.StatusBadRequest, "valid id and name are required")
	}
	if err := s.store.RenameFolder(r.Context(), p.Ref, id, in.Name); err != nil {
		return mapErr(err)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) deleteFolders(w http.ResponseWriter, r *http.Request) error {
	p, err := s.loadProject(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	if err := s.store.DeleteFolders(r.Context(), p.Ref, idsParam(r)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}
