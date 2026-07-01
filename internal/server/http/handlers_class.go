package http

import (
	"encoding/json"
	"io"
	"net/http"
)

func (s *Server) handleCreateClass(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.engine.CreateClass(req.Name); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name})
}

func (s *Server) handleListClasses(w http.ResponseWriter, r *http.Request) {
	classes := s.engine.ListClasses()
	writeJSON(w, http.StatusOK, map[string]any{"classes": classes})
}

func (s *Server) handleGetClass(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("class")
	if !s.engine.ClassExists(name) {
		writeError(w, http.StatusNotFound, "class does not exist")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name})
}

func (s *Server) handleDeleteClass(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("class")
	if err := s.engine.DropClass(name); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
