package http

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SQL execution strategy (no secondary indexes yet):
//
//   - The FROM class is scanned in STREAMING via the engine's keyset pages
//     (QueryPage with after=<last id>), so peak memory stays page + overlay
//     even over multi-million-row classes.
//   - WHERE conjuncts that touch only the base table are PUSHED DOWN into the
//     scan predicate, so non-matching rows are discarded before any join work.
//   - Each JOIN must be an equi-join (ON a.x = b.y). The joined class is loaded
//     once into a hash map keyed by the join field (parents are usually small);
//     each base row then probes in O(1). LEFT JOIN null-extends on miss; RIGHT
//     JOIN tracks matched build rows and emits the unmatched ones at the end.
//   - Without ORDER BY / RIGHT JOIN the scan stops as soon as OFFSET+LIMIT rows
//     were produced. With ORDER BY all matching rows are collected, sorted with
//     typed comparison, then sliced.
//
// String equality and LIKE fold case and accents by default (same rules as the
// REST filters; ci=false / ai=false on the /v1/query URL opt out).

// rowEnv maps a table alias to its current row (nil = null-extended row).
type rowEnv map[string]map[string]any

type sqlExec struct {
	s         *Server
	project   string
	fo        foldOpts
	baseAlias string
	reCache   map[string]*regexp.Regexp
}

// sqlJoinRun is a planned hash join.
type sqlJoinRun struct {
	clause  joinClause
	buildEx sqlExpr // key over the joined table's own row
	probeEx sqlExpr // key over the already-assembled env
	rows    []map[string]any
	index   map[string][]int // folded key -> row indices
	matched []bool           // right join: build rows seen at least once
}

const sqlScanPage = 2048

// execSQL runs a parsed SELECT and returns the projected rows.
func (s *Server) execSQL(reqProject string, st *selectStmt, fo foldOpts) ([]map[string]any, error) {
	x := &sqlExec{s: s, fo: fo, baseAlias: st.from.alias, reCache: map[string]*regexp.Regexp{}}
	x.project = st.from.project
	if x.project == "" {
		x.project = reqProject
	}

	limit := st.limit
	if limit < 0 {
		limit = 100 // default page: never dump an unbounded class in one response
	}

	// Plan joins: resolve equi-join sides and build the hash table per join.
	joins := make([]*sqlJoinRun, 0, len(st.joins))
	known := map[string]bool{st.from.alias: true}
	hasRight := false
	for _, jc := range st.joins {
		jr, err := x.planJoin(jc, known)
		if err != nil {
			return nil, err
		}
		known[jc.table.alias] = true
		if jc.kind == "right" {
			hasRight = true
		}
		joins = append(joins, jr)
	}

	// Split WHERE into pushable (base-only) and residual conjuncts.
	conjuncts := splitAnd(st.where)
	var pushed, residual []sqlExpr
	for _, c := range conjuncts {
		if !hasRight && x.basePushable(c) {
			pushed = append(pushed, c)
		} else {
			residual = append(residual, c)
		}
	}
	var pushMatch func([]byte) (bool, error)
	if len(pushed) > 0 {
		pushMatch = func(stored []byte) (bool, error) {
			obj, err := storedToObject(stored, 0)
			if err != nil {
				return false, err
			}
			delete(obj, "id") // id 0 is a placeholder, keep it invisible
			env := rowEnv{x.baseAlias: obj}
			for _, c := range pushed {
				v, err := x.eval(c, env)
				if err != nil {
					return false, err
				}
				if v != true {
					return false, nil
				}
			}
			return true, nil
		}
	}

	needAll := hasRight || len(st.orderBy) > 0
	var envs []rowEnv
	emitted := 0
	want := st.offset + limit

	// Stream the base class in keyset pages.
	after := int64(0)
	for {
		objs, err := s.engine.QueryPage(x.project, st.from.class, pushMatch, sqlScanPage, 0, after)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			obj, err := storedToObject(o.Data, o.ID)
			if err != nil {
				return nil, err
			}
			stop, err := x.expandJoins(rowEnv{x.baseAlias: obj}, joins, 0, func(env rowEnv) (bool, error) {
				ok, err := x.passWhere(residual, env)
				if err != nil || !ok {
					return false, err
				}
				envs = append(envs, env)
				emitted++
				return !needAll && emitted >= want, nil
			})
			if err != nil {
				return nil, err
			}
			if stop {
				goto scanned
			}
		}
		if len(objs) < sqlScanPage {
			break
		}
		after = objs[len(objs)-1].ID
	}
scanned:

	// RIGHT JOIN: emit build rows never matched, with every other alias null.
	// The full WHERE runs on these null-extended rows (pushdown was disabled).
	for _, jr := range joins {
		if jr.clause.kind != "right" {
			continue
		}
		for i, row := range jr.rows {
			if jr.matched[i] {
				continue
			}
			env := rowEnv{x.baseAlias: nil, jr.clause.table.alias: row}
			for _, other := range joins {
				if other != jr {
					env[other.clause.table.alias] = nil
				}
			}
			ok, err := x.passWhere(conjuncts, env)
			if err != nil {
				return nil, err
			}
			if ok {
				envs = append(envs, env)
			}
		}
	}

	// ORDER BY: typed sort over all collected rows (keys move with the rows).
	if len(st.orderBy) > 0 {
		type sortRow struct {
			env  rowEnv
			keys []any
		}
		rows := make([]sortRow, len(envs))
		for i, env := range envs {
			ks := make([]any, len(st.orderBy))
			for j, oi := range st.orderBy {
				v, err := x.eval(oi.expr, env)
				if err != nil {
					return nil, err
				}
				ks[j] = v
			}
			rows[i] = sortRow{env: env, keys: ks}
		}
		sort.SliceStable(rows, func(a, b int) bool {
			for j, oi := range st.orderBy {
				c := x.orderCompare(rows[a].keys[j], rows[b].keys[j])
				if c == 0 {
					continue
				}
				if oi.desc {
					return c > 0
				}
				return c < 0
			}
			return false
		})
		for i := range rows {
			envs[i] = rows[i].env
		}
	}

	// OFFSET / LIMIT over the final ordering.
	if st.offset > 0 {
		if st.offset >= len(envs) {
			envs = nil
		} else {
			envs = envs[st.offset:]
		}
	}
	if limit > 0 && len(envs) > limit {
		envs = envs[:limit]
	}

	// Projection.
	out := make([]map[string]any, 0, len(envs))
	for _, env := range envs {
		row, err := x.projectRow(st, joins, env)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// planJoin validates ON as an equi-join and loads + hashes the joined class.
func (x *sqlExec) planJoin(jc joinClause, known map[string]bool) (*sqlJoinRun, error) {
	project := jc.table.project
	if project == "" {
		project = x.project
	}
	eq, ok := jc.on.(binExpr)
	if !ok || eq.op != "=" {
		return nil, fmt.Errorf("sql: JOIN %s: ON must be an equality (a.x = b.y)", jc.table.class)
	}
	newAlias := jc.table.alias
	lA, rA := x.aliasesOf(eq.l), x.aliasesOf(eq.r)
	var buildEx, probeEx sqlExpr
	switch {
	case onlyAlias(lA, newAlias) && !rA[newAlias]:
		buildEx, probeEx = eq.l, eq.r
	case onlyAlias(rA, newAlias) && !lA[newAlias]:
		buildEx, probeEx = eq.r, eq.l
	default:
		return nil, fmt.Errorf("sql: JOIN %s: one side of ON must reference only %q and the other only prior tables", jc.table.class, newAlias)
	}
	for a := range x.aliasesOf(probeEx) {
		if !known[a] {
			return nil, fmt.Errorf("sql: JOIN %s: unknown alias %q in ON", jc.table.class, a)
		}
	}

	objs, err := x.s.engine.QueryObjects(project, jc.table.class, nil, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("sql: JOIN %s: %w", jc.table.class, err)
	}
	jr := &sqlJoinRun{clause: jc, buildEx: buildEx, probeEx: probeEx,
		rows: make([]map[string]any, 0, len(objs)), index: map[string][]int{}}
	for _, o := range objs {
		row, err := storedToObject(o.Data, o.ID)
		if err != nil {
			return nil, err
		}
		v, err := x.eval(buildEx, rowEnv{newAlias: row})
		if err != nil {
			return nil, err
		}
		if k, ok := x.foldKey(v); ok {
			jr.index[k] = append(jr.index[k], len(jr.rows))
		}
		jr.rows = append(jr.rows, row)
	}
	jr.matched = make([]bool, len(jr.rows))
	return jr, nil
}

// expandJoins probes joins[i:] against env, invoking fn for every combined row.
// fn returns stop=true to abort the whole scan (limit reached).
func (x *sqlExec) expandJoins(env rowEnv, joins []*sqlJoinRun, i int, fn func(rowEnv) (bool, error)) (bool, error) {
	if i >= len(joins) {
		return fn(env)
	}
	jr := joins[i]
	alias := jr.clause.table.alias
	v, err := x.eval(jr.probeEx, env)
	if err != nil {
		return false, err
	}
	var hits []int
	if k, ok := x.foldKey(v); ok {
		hits = jr.index[k]
	}
	if len(hits) == 0 {
		switch jr.clause.kind {
		case "inner", "right": // no combined row from this base row
			return false, nil
		case "left":
			next := make(rowEnv, len(env)+1)
			for k2, v2 := range env {
				next[k2] = v2
			}
			next[alias] = nil
			return x.expandJoins(next, joins, i+1, fn)
		}
	}
	for _, h := range hits {
		jr.matched[h] = true
		next := make(rowEnv, len(env)+1)
		for k2, v2 := range env {
			next[k2] = v2
		}
		next[alias] = jr.rows[h]
		stop, err := x.expandJoins(next, joins, i+1, fn)
		if err != nil || stop {
			return stop, err
		}
	}
	return false, nil
}

func (x *sqlExec) passWhere(conjuncts []sqlExpr, env rowEnv) (bool, error) {
	for _, c := range conjuncts {
		v, err := x.eval(c, env)
		if err != nil {
			return false, err
		}
		if v != true {
			return false, nil
		}
	}
	return true, nil
}

// projectRow builds the output row for env per the select list.
func (x *sqlExec) projectRow(st *selectStmt, joins []*sqlJoinRun, env rowEnv) (map[string]any, error) {
	out := map[string]any{}
	if st.star {
		if base := env[x.baseAlias]; base != nil {
			for k, v := range base {
				out[k] = v
			}
		}
		for _, jr := range joins {
			a := jr.clause.table.alias
			if r, ok := env[a]; ok && r != nil {
				out[a] = r
			} else {
				out[a] = nil
			}
		}
		return out, nil
	}
	for i, item := range st.items {
		if item.starAlias != "" {
			key := item.starAlias
			if item.as != "" {
				key = item.as
			}
			if r, ok := env[item.starAlias]; ok {
				out[key] = r
			} else {
				return nil, fmt.Errorf("sql: unknown alias %q in select list", item.starAlias)
			}
			continue
		}
		v, err := x.eval(item.expr, env)
		if err != nil {
			return nil, err
		}
		out[x.outputName(item, i)] = v
	}
	return out, nil
}

func (x *sqlExec) outputName(item selectItem, i int) string {
	if item.as != "" {
		return item.as
	}
	return exprName(item.expr, i)
}

func exprName(e sqlExpr, i int) string {
	switch t := e.(type) {
	case colRef:
		return t.field
	case castExpr:
		return exprName(t.e, i)
	case callExpr:
		return strings.ToLower(t.fn)
	default:
		return "col" + strconv.Itoa(i+1)
	}
}

// ---- expression evaluation ----

func (x *sqlExec) eval(e sqlExpr, env rowEnv) (any, error) {
	switch t := e.(type) {
	case literal:
		return t.val, nil
	case colRef:
		alias := t.alias
		if alias == "" {
			alias = x.baseAlias
		}
		obj, ok := env[alias]
		if !ok {
			return nil, fmt.Errorf("sql: unknown table alias %q", t.alias)
		}
		if obj == nil {
			return nil, nil // null-extended row
		}
		return obj[t.field], nil
	case binExpr:
		switch t.op {
		case "AND":
			lv, err := x.eval(t.l, env)
			if err != nil {
				return nil, err
			}
			if lv != true {
				return false, nil
			}
			rv, err := x.eval(t.r, env)
			if err != nil {
				return nil, err
			}
			return rv == true, nil
		case "OR":
			lv, err := x.eval(t.l, env)
			if err != nil {
				return nil, err
			}
			if lv == true {
				return true, nil
			}
			rv, err := x.eval(t.r, env)
			if err != nil {
				return nil, err
			}
			return rv == true, nil
		}
		lv, err := x.eval(t.l, env)
		if err != nil {
			return nil, err
		}
		rv, err := x.eval(t.r, env)
		if err != nil {
			return nil, err
		}
		c, ok := x.compare(lv, rv)
		if !ok {
			return false, nil // NULL or incomparable never matches
		}
		switch t.op {
		case "=":
			return c == 0, nil
		case "<>":
			return c != 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		case ">=":
			return c >= 0, nil
		}
		return nil, fmt.Errorf("sql: unsupported operator %q", t.op)
	case notExpr:
		v, err := x.eval(t.e, env)
		if err != nil {
			return nil, err
		}
		return v != true, nil
	case likeExpr:
		lv, err := x.eval(t.l, env)
		if err != nil {
			return nil, err
		}
		rv, err := x.eval(t.r, env)
		if err != nil {
			return nil, err
		}
		ls, ok1 := fieldString(lv)
		ps, ok2 := fieldString(rv)
		if !ok1 || !ok2 {
			return false, nil
		}
		re, err := x.likeRegex(x.fo.fold(ps))
		if err != nil {
			return nil, err
		}
		m := re.MatchString(x.fo.fold(ls))
		return m != t.neg, nil
	case inExpr:
		v, err := x.eval(t.e, env)
		if err != nil {
			return nil, err
		}
		found := false
		for _, le := range t.list {
			lv, err := x.eval(le, env)
			if err != nil {
				return nil, err
			}
			if c, ok := x.compare(v, lv); ok && c == 0 {
				found = true
				break
			}
		}
		return found != t.neg, nil
	case isNullExpr:
		v, err := x.eval(t.e, env)
		if err != nil {
			return nil, err
		}
		return (v == nil) != t.neg, nil
	case callExpr:
		switch t.fn {
		case "COALESCE":
			for _, a := range t.args {
				v, err := x.eval(a, env)
				if err != nil {
					return nil, err
				}
				if v != nil {
					return v, nil
				}
			}
			return nil, nil
		case "UPPER", "LOWER":
			if len(t.args) != 1 {
				return nil, fmt.Errorf("sql: %s takes exactly one argument", t.fn)
			}
			v, err := x.eval(t.args[0], env)
			if err != nil {
				return nil, err
			}
			s, ok := fieldString(v)
			if !ok {
				return nil, nil
			}
			if t.fn == "UPPER" {
				return strings.ToUpper(s), nil
			}
			return strings.ToLower(s), nil
		}
		return nil, fmt.Errorf("sql: unknown function %q", t.fn)
	case castExpr:
		v, err := x.eval(t.e, env)
		if err != nil {
			return nil, err
		}
		return castVal(v, t.typ)
	}
	return nil, fmt.Errorf("sql: cannot evaluate expression %T", e)
}

func (x *sqlExec) likeRegex(foldedPattern string) (*regexp.Regexp, error) {
	if re, ok := x.reCache[foldedPattern]; ok {
		return re, nil
	}
	re, err := likeToRegex(foldedPattern)
	if err != nil {
		return nil, fmt.Errorf("sql: invalid LIKE pattern: %w", err)
	}
	x.reCache[foldedPattern] = re
	return re, nil
}

// compare orders two values with type awareness: numbers numerically (a
// numeric string next to a number is parsed), dates chronologically, strings
// with case/accent folding. Returns ok=false when either side is NULL or the
// values are incomparable.
func (x *sqlExec) compare(a, b any) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	// times (produced by CAST ... AS DATE) compare chronologically
	at, aIsT := a.(time.Time)
	bt, bIsT := b.(time.Time)
	if aIsT || bIsT {
		if !aIsT {
			t, ok := parseTimeAny(a)
			if !ok {
				return 0, false
			}
			at = t
		}
		if !bIsT {
			t, ok := parseTimeAny(b)
			if !ok {
				return 0, false
			}
			bt = t
		}
		return at.Compare(bt), true
	}
	// numbers (numeric strings coerce when the other side is a native number)
	af, aNum := toFloat(a)
	bf, bNum := toFloat(b)
	if aNum && bNum {
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		}
		return 0, true
	}
	if aNum != bNum { // one native number, other side maybe numeric string
		if s, ok := otherAsFloat(a, b, aNum); ok {
			var af2, bf2 float64
			if aNum {
				af2, bf2 = af, s
			} else {
				af2, bf2 = s, bf
			}
			switch {
			case af2 < bf2:
				return -1, true
			case af2 > bf2:
				return 1, true
			}
			return 0, true
		}
	}
	// fallback: folded string comparison
	as, ok1 := fieldString(a)
	bs, ok2 := fieldString(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return strings.Compare(x.fo.fold(as), x.fo.fold(bs)), true
}

// orderCompare is compare with NULLS LAST semantics for ORDER BY.
func (x *sqlExec) orderCompare(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	if c, ok := x.compare(a, b); ok {
		return c
	}
	return 0
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case uint64:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	}
	return 0, false
}

// otherAsFloat parses the non-numeric side as a float when possible.
func otherAsFloat(a, b any, aIsNum bool) (float64, bool) {
	v := a
	if aIsNum {
		v = b
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

var sqlTimeLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02",
	"02/01/2006", // pt-BR
}

func parseTimeAny(v any) (time.Time, bool) {
	if t, ok := v.(time.Time); ok {
		return t, true
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	for _, layout := range sqlTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// castVal implements CAST(v AS type).
func castVal(v any, typ string) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch typ {
	case "INT", "INTEGER", "BIGINT", "SMALLINT":
		if f, ok := toFloat(v); ok {
			return int64(f), nil
		}
		if s, ok := v.(string); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return n, nil
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return int64(f), nil
			}
		}
		if b, ok := v.(bool); ok {
			if b {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case "FLOAT", "DOUBLE", "REAL", "NUMERIC", "DECIMAL":
		if f, ok := toFloat(v); ok {
			return f, nil
		}
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f, nil
			}
		}
	case "STRING", "VARCHAR", "CHAR", "TEXT":
		if s, ok := fieldString(v); ok {
			return s, nil
		}
	case "BOOL", "BOOLEAN":
		switch t := v.(type) {
		case bool:
			return t, nil
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(strings.ToLower(t))); err == nil {
				return b, nil
			}
		default:
			if f, ok := toFloat(v); ok {
				return f != 0, nil
			}
		}
	case "DATE", "DATETIME", "TIMESTAMP":
		if t, ok := parseTimeAny(v); ok {
			return t, nil
		}
	default:
		return nil, fmt.Errorf("sql: unknown CAST type %q", typ)
	}
	return nil, fmt.Errorf("sql: cannot cast %v (%T) to %s", v, v, typ)
}

// ---- planning helpers ----

// splitAnd flattens a WHERE tree into its top-level AND conjuncts.
func splitAnd(e sqlExpr) []sqlExpr {
	if e == nil {
		return nil
	}
	if b, ok := e.(binExpr); ok && b.op == "AND" {
		return append(splitAnd(b.l), splitAnd(b.r)...)
	}
	return []sqlExpr{e}
}

// aliasesOf collects the table aliases referenced by an expression (a bare
// column counts as the base alias).
func (x *sqlExec) aliasesOf(e sqlExpr) map[string]bool {
	out := map[string]bool{}
	var walk func(sqlExpr)
	walk = func(e sqlExpr) {
		switch t := e.(type) {
		case colRef:
			a := t.alias
			if a == "" {
				a = x.baseAlias
			}
			out[a] = true
		case binExpr:
			walk(t.l)
			walk(t.r)
		case notExpr:
			walk(t.e)
		case likeExpr:
			walk(t.l)
			walk(t.r)
		case inExpr:
			walk(t.e)
			for _, le := range t.list {
				walk(le)
			}
		case isNullExpr:
			walk(t.e)
		case callExpr:
			for _, a := range t.args {
				walk(a)
			}
		case castExpr:
			walk(t.e)
		}
	}
	walk(e)
	return out
}

// basePushable reports whether a conjunct can run inside the base scan: it must
// reference only the base table and not the synthetic "id" field (the scan
// predicate sees the stored payload, which does not carry the id).
func (x *sqlExec) basePushable(e sqlExpr) bool {
	for a := range x.aliasesOf(e) {
		if a != x.baseAlias {
			return false
		}
	}
	return !refsField(e, x.baseAlias, "id")
}

// refsField reports whether e references alias.field (bare columns count as
// base-alias columns).
func refsField(e sqlExpr, alias, field string) bool {
	found := false
	var walk func(sqlExpr)
	walk = func(e sqlExpr) {
		switch t := e.(type) {
		case colRef:
			a := t.alias
			if a == "" {
				a = alias
			}
			if a == alias && t.field == field {
				found = true
			}
		case binExpr:
			walk(t.l)
			walk(t.r)
		case notExpr:
			walk(t.e)
		case likeExpr:
			walk(t.l)
			walk(t.r)
		case inExpr:
			walk(t.e)
			for _, le := range t.list {
				walk(le)
			}
		case isNullExpr:
			walk(t.e)
		case callExpr:
			for _, a := range t.args {
				walk(a)
			}
		case castExpr:
			walk(t.e)
		}
	}
	walk(e)
	return found
}

func onlyAlias(set map[string]bool, alias string) bool {
	return len(set) == 1 && set[alias]
}

// foldKey renders a join-key value into its canonical hash form: numbers via
// fieldString (so 24 and "24" collide as intended), strings folded.
func (x *sqlExec) foldKey(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	if f, ok := toFloat(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64), true
	}
	s, ok := fieldString(v)
	if !ok {
		return "", false
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return strconv.FormatFloat(f, 'f', -1, 64), true
	}
	return x.fo.fold(s), true
}
