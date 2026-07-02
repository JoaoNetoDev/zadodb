package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	maxObjectBytes = 8 << 20   // 8 MiB single-object request cap
	maxBulkBytes   = 256 << 20 // 256 MiB bulk request cap
)

func (s *Server) handleCreateObject(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	project := projectOf(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxObjectBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	stored, err := jsonToStored(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}
	id, err := s.engine.CreateObject(project, class, stored)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	obj, err := storedToObject(stored, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, obj)
}

// maxBulkObjects caps how many objects one bulk request may carry.
const maxBulkObjects = 10000

func (s *Server) handleCreateObjectsBulk(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	project := projectOf(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBulkBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON array of objects: "+err.Error())
		return
	}
	if len(items) == 0 {
		writeJSON(w, http.StatusCreated, map[string]any{"ids": []int64{}, "count": 0})
		return
	}
	if len(items) > maxBulkObjects {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("too many objects: %d > %d", len(items), maxBulkObjects))
		return
	}

	datas := make([][]byte, len(items))
	for i, it := range items {
		stored, err := jsonToStored(it)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("item %d is not a JSON object: %v", i, err))
			return
		}
		datas[i] = stored
	}

	ids, err := s.engine.CreateObjectsBulk(project, class, datas)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ids": ids, "count": len(ids)})
}

func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	project := projectOf(r)
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	stored, found, err := s.engine.GetObject(project, class, id)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	obj, err := storedToObject(stored, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	project := projectOf(r)
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxObjectBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	stored, err := jsonToStored(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON object: "+err.Error())
		return
	}
	if err := s.engine.PutObject(project, class, id, stored); err != nil {
		writeEngineError(w, err)
		return
	}
	obj, err := storedToObject(stored, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	project := projectOf(r)
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := s.engine.DeleteObject(project, class, id); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	project := projectOf(r)
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 100)
	offset := parseIntDefault(q.Get("offset"), 0)
	after := int64(parseIntDefault(q.Get("after"), 0)) // keyset cursor: ids > after

	fo := parseFoldOpts(q)
	var baseSpecs, relSpecs []filterSpec
	for _, sp := range parseFilterSpecs(q) {
		if sp.alias == "" {
			baseSpecs = append(baseSpecs, sp)
		} else {
			relSpecs = append(relSpecs, sp)
		}
	}
	baseMatcher, err := buildMatcher(baseSpecs, fo)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var match func([]byte) (bool, error)
	switch {
	case len(relSpecs) > 0:
		pred, err := s.buildJoinPredicate(project, class, baseMatcher, relSpecs, fo)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		match = pred
	case baseMatcher != nil:
		match = baseMatcher.match
	}

	objs, err := s.engine.QueryPage(project, class, match, limit, offset, after)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		obj, err := storedToObject(o.Data, o.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, obj)
	}
	// Embed related parent objects when requested: ?include=municipio,uf
	if inc := parseCSV(q.Get("include")); len(inc) > 0 {
		if err := s.attachIncludes(project, class, out, inc); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	resp := map[string]any{
		"objects": out,
		"count":   len(out),
		"limit":   limit,
		"offset":  offset,
	}
	// Keyset cursor: when a full page came back, echo the last id so the client
	// can fetch the next page with ?after=<next_after> (O(page), no offset walk).
	if limit > 0 && len(objs) == limit {
		resp["next_after"] = objs[len(objs)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// projectHeader is the HTTP header carrying the virtual namespace ("project").
// Absent or empty selects the default project, which uses the legacy key layout
// (so pre-project clients keep working unchanged).
const projectHeader = "X-Zado-Project"

func projectOf(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(projectHeader))
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid object id")
		return 0, false
	}
	return id, true
}

// parseCSV splits a comma-separated query value, trimming spaces and dropping
// empties (e.g. include=municipio,uf).
func parseCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
