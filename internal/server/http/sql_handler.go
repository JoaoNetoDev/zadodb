package http

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// handleQuery runs a SQL SELECT (see sql_parser.go for the supported subset).
//
//	POST /v1/query
//	Content-Type: application/json  -> {"sql": "SELECT ..."}
//	any other content type          -> the raw body is the SQL text
//
// The project comes from the X-Zado-Project header, or a project-qualified
// table name (FROM projeto.classe) overrides it per table. ci=false / ai=false
// URL params opt out of case/accent folding.
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	sqlText := strings.TrimSpace(string(body))
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") || strings.HasPrefix(sqlText, "{") {
		var req struct {
			SQL string `json:"sql"`
		}
		if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.SQL) == "" {
			writeError(w, http.StatusBadRequest, `body must be {"sql": "SELECT ..."}`)
			return
		}
		sqlText = req.SQL
	}
	if sqlText == "" {
		writeError(w, http.StatusBadRequest, "empty query")
		return
	}

	st, err := parseSQL(sqlText)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fo := parseFoldOpts(r.URL.Query())
	rows, err := s.execSQL(projectOf(r), st, fo)
	if err != nil {
		if strings.HasPrefix(err.Error(), "sql:") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":  rows,
		"count": len(rows),
	})
}
