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
	if err := s.engine.CreateClass(projectOf(r), req.Name); err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": req.Name, "project": projectOf(r)})
}

func (s *Server) handleListClasses(w http.ResponseWriter, r *http.Request) {
	project := projectOf(r)
	classes := s.engine.ListClasses(project)
	writeJSON(w, http.StatusOK, map[string]any{"classes": classes, "project": project})
}

func (s *Server) handleGetClass(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("class")
	project := projectOf(r)
	if !s.engine.ClassExists(project, name) {
		writeError(w, http.StatusNotFound, "class does not exist")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "project": project})
}

func (s *Server) handleDeleteClass(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("class")
	if err := s.engine.DropClass(projectOf(r), name); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListProjects returns the distinct project namespaces that hold classes.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"projects": s.engine.ListProjects()})
}
