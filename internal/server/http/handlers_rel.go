package http

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
)

// handleCreateRelationship registers a foreign key on a class:
//
//	POST /v1/classes/{class}/relationships
//	{ "name":"municipio", "localField":"municipioCodigo",
//	  "toClass":"municipio", "remoteField":"codigoIbge" }
//
// name is optional (defaults to toClass) and is how queries reference the
// relation, e.g. eq.municipio.nome=...
func (s *Server) handleCreateRelationship(w http.ResponseWriter, r *http.Request) {
	fromClass := r.PathValue("class")
	project := projectOf(r)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	var rel storage.Relationship
	if err := json.Unmarshal(body, &rel); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := s.engine.CreateRelationship(project, fromClass, rel); err != nil {
		writeEngineError(w, err)
		return
	}
	if rel.Name == "" {
		rel.Name = rel.ToClass
	}
	writeJSON(w, http.StatusCreated, rel)
}

// handleListRelationships lists the relationships declared on a class.
func (s *Server) handleListRelationships(w http.ResponseWriter, r *http.Request) {
	fromClass := r.PathValue("class")
	project := projectOf(r)
	if !s.engine.ClassExists(project, fromClass) {
		writeError(w, http.StatusNotFound, "class does not exist")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"relationships": s.engine.ListRelationships(project, fromClass),
	})
}

// handleDeleteRelationship removes a relationship by name.
func (s *Server) handleDeleteRelationship(w http.ResponseWriter, r *http.Request) {
	fromClass := r.PathValue("class")
	name := r.PathValue("name")
	if err := s.engine.DropRelationship(projectOf(r), fromClass, name); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
