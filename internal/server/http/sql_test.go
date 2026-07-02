package http

import (
	"net/http"
	"testing"
)

// query posts SQL to /v1/query and returns the decoded response.
func query(t *testing.T, base, sql string) (*http.Response, map[string]any) {
	t.Helper()
	return do(t, "POST", base+"/v1/query", map[string]any{"sql": sql})
}

func rowsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["rows"].([]any)
	if !ok {
		t.Fatalf("no rows in response: %v", m)
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func TestHTTPSQLQuery(t *testing.T) {
	ts := newTestServer(t)
	base := ts.URL

	for _, c := range []string{"uf", "municipio", "logradouro"} {
		do(t, "POST", base+"/v1/classes", map[string]any{"name": c})
	}
	do(t, "POST", base+"/v1/classes/uf/objects", map[string]any{"codigoUf": 24, "sigla": "RN", "nome": "Rio Grande do Norte"})
	do(t, "POST", base+"/v1/classes/uf/objects", map[string]any{"codigoUf": 35, "sigla": "SP", "nome": "São Paulo"})
	do(t, "POST", base+"/v1/classes/municipio/objects", map[string]any{"codigoIbge": 2408102, "codigoUf": 24, "nome": "Mossoró"})
	do(t, "POST", base+"/v1/classes/municipio/objects", map[string]any{"codigoIbge": 2411403, "codigoUf": 24, "nome": "Natal"})
	do(t, "POST", base+"/v1/classes/municipio/objects", map[string]any{"codigoIbge": 3550308, "codigoUf": 35, "nome": "São Paulo"})
	do(t, "POST", base+"/v1/classes/municipio/objects", map[string]any{"codigoIbge": 2401403, "codigoUf": 24, "nome": "Caicó"}) // no logradouros
	do(t, "POST", base+"/v1/classes/logradouro/objects", map[string]any{"municipioCodigo": 2408102, "nome": "RUA A", "cep": "59600-000"})
	do(t, "POST", base+"/v1/classes/logradouro/objects", map[string]any{"municipioCodigo": 2408102, "nome": "RUA B"})
	do(t, "POST", base+"/v1/classes/logradouro/objects", map[string]any{"municipioCodigo": 2411403, "nome": "RUA C"})
	do(t, "POST", base+"/v1/classes/logradouro/objects", map[string]any{"municipioCodigo": 3550308, "nome": "RUA D"})
	do(t, "POST", base+"/v1/classes/logradouro/objects", map[string]any{"municipioCodigo": 9999999, "nome": "RUA ORFA"}) // orphan FK

	t.Run("select star with folding like", func(t *testing.T) {
		resp, m := query(t, base, "SELECT * FROM logradouro WHERE nome LIKE 'rua%'")
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d: %v", resp.StatusCode, m)
		}
		if len(rowsOf(t, m)) != 5 {
			t.Fatalf("count = %v, want 5", m["count"])
		}
	})

	t.Run("two-hop join with folding and order", func(t *testing.T) {
		resp, m := query(t, base, `
			SELECT l.nome, m.nome AS cidade, u.sigla
			FROM logradouro l
			JOIN municipio m ON l.municipioCodigo = m.codigoIbge
			JOIN uf u ON m.codigoUf = u.codigoUf
			WHERE u.sigla = 'rn' AND m.nome LIKE 'mossoro%'
			ORDER BY l.nome DESC`)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d: %v", resp.StatusCode, m)
		}
		rows := rowsOf(t, m)
		if len(rows) != 2 || rows[0]["nome"] != "RUA B" || rows[1]["nome"] != "RUA A" {
			t.Fatalf("rows = %v", rows)
		}
		if rows[0]["cidade"] != "Mossoró" || rows[0]["sigla"] != "RN" {
			t.Fatalf("projection = %v", rows[0])
		}
	})

	t.Run("left join with coalesce and is null", func(t *testing.T) {
		resp, m := query(t, base, `
			SELECT l.nome, COALESCE(m.nome, 'sem cidade') AS cidade
			FROM logradouro l
			LEFT JOIN municipio m ON l.municipioCodigo = m.codigoIbge
			WHERE m.codigoIbge IS NULL`)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d: %v", resp.StatusCode, m)
		}
		rows := rowsOf(t, m)
		if len(rows) != 1 || rows[0]["nome"] != "RUA ORFA" || rows[0]["cidade"] != "sem cidade" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("right join emits unmatched municipios", func(t *testing.T) {
		resp, m := query(t, base, `
			SELECT m.nome AS cidade, l.nome AS rua
			FROM logradouro l
			RIGHT JOIN municipio m ON l.municipioCodigo = m.codigoIbge
			WHERE l.nome IS NULL`)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d: %v", resp.StatusCode, m)
		}
		rows := rowsOf(t, m)
		if len(rows) != 1 || rows[0]["cidade"] != "Caicó" || rows[0]["rua"] != nil {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("cast, in and numeric typing", func(t *testing.T) {
		resp, m := query(t, base, `
			SELECT l.nome
			FROM logradouro l
			JOIN municipio m ON l.municipioCodigo = m.codigoIbge
			WHERE CAST(m.codigoUf AS INT) = 24 AND m.nome IN ('Natal', 'mossoró')
			ORDER BY l.nome`)
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d: %v", resp.StatusCode, m)
		}
		rows := rowsOf(t, m)
		if len(rows) != 3 || rows[0]["nome"] != "RUA A" || rows[2]["nome"] != "RUA C" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("first and limit offset", func(t *testing.T) {
		_, m := query(t, base, "SELECT FIRST 2 nome FROM logradouro ORDER BY nome")
		if rows := rowsOf(t, m); len(rows) != 2 || rows[0]["nome"] != "RUA A" {
			t.Fatalf("FIRST rows = %v", rows)
		}
		_, m = query(t, base, "SELECT nome FROM logradouro ORDER BY nome LIMIT 2 OFFSET 1")
		if rows := rowsOf(t, m); len(rows) != 2 || rows[0]["nome"] != "RUA B" {
			t.Fatalf("LIMIT OFFSET rows = %v", rows)
		}
	})

	t.Run("comparison operators and order by number", func(t *testing.T) {
		_, m := query(t, base, "SELECT nome, municipioCodigo FROM logradouro WHERE municipioCodigo > 2408102 AND municipioCodigo <> 9999999 ORDER BY municipioCodigo DESC")
		rows := rowsOf(t, m)
		if len(rows) != 2 || rows[0]["nome"] != "RUA D" || rows[1]["nome"] != "RUA C" {
			t.Fatalf("rows = %v", rows)
		}
	})

	t.Run("parse error returns 400", func(t *testing.T) {
		resp, _ := query(t, base, "SELECT FROM WHERE")
		if resp.StatusCode != 400 {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown class returns 404", func(t *testing.T) {
		resp, _ := query(t, base, "SELECT * FROM nao_existe")
		if resp.StatusCode != 404 {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}
