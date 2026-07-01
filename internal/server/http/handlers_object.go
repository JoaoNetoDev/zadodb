package http

import (
	"io"
	"net/http"
	"strconv"
)

const maxObjectBytes = 8 << 20 // 8 MiB request cap

func (s *Server) handleCreateObject(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
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
	id, err := s.engine.CreateObject(class, stored)
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

func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	stored, found, err := s.engine.GetObject(class, id)
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
	if err := s.engine.PutObject(class, id, stored); err != nil {
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
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := s.engine.DeleteObject(class, id); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListObjects(w http.ResponseWriter, r *http.Request) {
	class := r.PathValue("class")
	limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
	offset := parseIntDefault(r.URL.Query().Get("offset"), 0)

	objs, err := s.engine.ListObjects(class, limit, offset)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": out,
		"count":   len(out),
		"limit":   limit,
		"offset":  offset,
	})
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid object id")
		return 0, false
	}
	return id, true
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
