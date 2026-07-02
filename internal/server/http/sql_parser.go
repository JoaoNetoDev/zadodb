package http

import (
	"fmt"
	"strconv"
	"strings"
)

// SQL subset supported by POST /v1/query:
//
//	SELECT [FIRST n] * | expr [AS name] [, ...]
//	FROM [project.]class [[AS] alias]
//	[ [INNER|LEFT|RIGHT] JOIN [project.]class [[AS] alias] ON expr ]...
//	[ WHERE expr ]
//	[ ORDER BY expr [ASC|DESC] [, ...] ]
//	[ LIMIT n [OFFSET m] ]
//
// Expressions: AND/OR/NOT, = <> != < <= > >=, LIKE / NOT LIKE, IN (...),
// IS [NOT] NULL, COALESCE(...), CAST(expr AS type), literals ('str', 123,
// TRUE, FALSE, NULL) and column refs (field or alias.field). String equality
// and LIKE are case- and accent-insensitive by default (same folding as the
// REST filters).

// ---- AST ----

type sqlExpr interface{}

type colRef struct{ alias, field string } // alias "" = base table
type literal struct{ val any }
type binExpr struct {
	op   string // OR AND = <> < <= > >=
	l, r sqlExpr
}
type notExpr struct{ e sqlExpr }
type likeExpr struct {
	l, r sqlExpr
	neg  bool
}
type inExpr struct {
	e    sqlExpr
	list []sqlExpr
	neg  bool
}
type isNullExpr struct {
	e   sqlExpr
	neg bool
}
type callExpr struct {
	fn   string // COALESCE, UPPER, LOWER
	args []sqlExpr
}
type castExpr struct {
	e   sqlExpr
	typ string // INT, FLOAT, STRING, BOOL, DATE
}

type selectItem struct {
	expr      sqlExpr
	as        string
	starAlias string // set for "alias.*"
}

type tableRef struct {
	project string // "" = request project (X-Zado-Project header)
	class   string
	alias   string // defaults to class name
}

type joinClause struct {
	kind  string // "inner", "left", "right"
	table tableRef
	on    sqlExpr
}

type orderItem struct {
	expr sqlExpr
	desc bool
}

type selectStmt struct {
	star    bool
	items   []selectItem
	from    tableRef
	joins   []joinClause
	where   sqlExpr
	orderBy []orderItem
	limit   int // -1 = not specified
	offset  int
}

// ---- Parser (recursive descent) ----

type sqlParser struct {
	toks []token
	pos  int
}

func parseSQL(src string) (*selectStmt, error) {
	toks, err := lexSQL(src)
	if err != nil {
		return nil, err
	}
	p := &sqlParser{toks: toks}
	st, err := p.parseSelect()
	if err != nil {
		return nil, err
	}
	if !p.atEOF() {
		return nil, p.errf("unexpected %q after end of statement", p.cur().text)
	}
	return st, nil
}

func (p *sqlParser) cur() token  { return p.toks[p.pos] }
func (p *sqlParser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *sqlParser) atEOF() bool { return p.cur().kind == tkEOF }

func (p *sqlParser) errf(format string, a ...any) error {
	return fmt.Errorf("sql: "+format+" (position %d)", append(a, p.cur().pos)...)
}

// isKw reports whether the current token is the given keyword (case-insensitive).
func (p *sqlParser) isKw(kw string) bool {
	t := p.cur()
	return t.kind == tkIdent && strings.EqualFold(t.text, kw)
}

func (p *sqlParser) acceptKw(kw string) bool {
	if p.isKw(kw) {
		p.pos++
		return true
	}
	return false
}

func (p *sqlParser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.errf("expected %s, got %q", kw, p.cur().text)
	}
	return nil
}

func (p *sqlParser) isOp(op string) bool {
	t := p.cur()
	return t.kind == tkOp && t.text == op
}

func (p *sqlParser) acceptOp(op string) bool {
	if p.isOp(op) {
		p.pos++
		return true
	}
	return false
}

func (p *sqlParser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errf("expected %q, got %q", op, p.cur().text)
	}
	return nil
}

var sqlKeywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "AND": true, "OR": true,
	"NOT": true, "LIKE": true, "IN": true, "IS": true, "NULL": true,
	"AS": true, "JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true,
	"OUTER": true, "ON": true, "ORDER": true, "BY": true, "ASC": true,
	"DESC": true, "LIMIT": true, "OFFSET": true, "FIRST": true,
	"TRUE": true, "FALSE": true, "CAST": true, "COALESCE": true,
}

func isKeyword(s string) bool { return sqlKeywords[strings.ToUpper(s)] }

func (p *sqlParser) parseSelect() (*selectStmt, error) {
	if err := p.expectKw("SELECT"); err != nil {
		return nil, err
	}
	st := &selectStmt{limit: -1}

	// FIRST n (Firebird-style row cap)
	if p.isKw("FIRST") && p.toks[p.pos+1].kind == tkNumber {
		p.pos++
		n, _ := strconv.Atoi(p.next().text)
		st.limit = n
	}

	// select list
	if p.acceptOp("*") {
		st.star = true
	} else {
		for {
			item, err := p.parseSelectItem()
			if err != nil {
				return nil, err
			}
			st.items = append(st.items, item)
			if !p.acceptOp(",") {
				break
			}
		}
	}

	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	from, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	st.from = from

	// joins
	for {
		kind := ""
		switch {
		case p.isKw("JOIN"):
			p.pos++
			kind = "inner"
		case p.isKw("INNER"):
			p.pos++
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			kind = "inner"
		case p.isKw("LEFT"):
			p.pos++
			p.acceptKw("OUTER")
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			kind = "left"
		case p.isKw("RIGHT"):
			p.pos++
			p.acceptKw("OUTER")
			if err := p.expectKw("JOIN"); err != nil {
				return nil, err
			}
			kind = "right"
		}
		if kind == "" {
			break
		}
		tbl, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("ON"); err != nil {
			return nil, err
		}
		on, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.joins = append(st.joins, joinClause{kind: kind, table: tbl, on: on})
	}

	if p.acceptKw("WHERE") {
		w, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.where = w
	}

	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			it := orderItem{expr: e}
			if p.acceptKw("DESC") {
				it.desc = true
			} else {
				p.acceptKw("ASC")
			}
			st.orderBy = append(st.orderBy, it)
			if !p.acceptOp(",") {
				break
			}
		}
	}

	if p.acceptKw("LIMIT") {
		if p.cur().kind != tkNumber {
			return nil, p.errf("expected number after LIMIT")
		}
		n, _ := strconv.Atoi(p.next().text)
		st.limit = n
		if p.acceptKw("OFFSET") {
			if p.cur().kind != tkNumber {
				return nil, p.errf("expected number after OFFSET")
			}
			m, _ := strconv.Atoi(p.next().text)
			st.offset = m
		}
	}
	return st, nil
}

func (p *sqlParser) parseSelectItem() (selectItem, error) {
	// alias.*
	if p.cur().kind == tkIdent && !isKeyword(p.cur().text) &&
		p.toks[p.pos+1].kind == tkOp && p.toks[p.pos+1].text == "." &&
		p.toks[p.pos+2].kind == tkOp && p.toks[p.pos+2].text == "*" {
		alias := p.next().text
		p.pos += 2
		return selectItem{starAlias: alias}, nil
	}
	e, err := p.parseExpr()
	if err != nil {
		return selectItem{}, err
	}
	item := selectItem{expr: e}
	if p.acceptKw("AS") {
		if p.cur().kind != tkIdent {
			return selectItem{}, p.errf("expected name after AS")
		}
		item.as = p.next().text
	} else if p.cur().kind == tkIdent && !isKeyword(p.cur().text) {
		item.as = p.next().text // bare alias
	}
	return item, nil
}

func (p *sqlParser) parseTableRef() (tableRef, error) {
	if p.cur().kind != tkIdent || isKeyword(p.cur().text) {
		return tableRef{}, p.errf("expected table name, got %q", p.cur().text)
	}
	t := tableRef{class: p.next().text}
	if p.acceptOp(".") { // project.class
		if p.cur().kind != tkIdent {
			return tableRef{}, p.errf("expected class name after project qualifier")
		}
		t.project, t.class = t.class, p.next().text
	}
	if p.acceptKw("AS") {
		if p.cur().kind != tkIdent {
			return tableRef{}, p.errf("expected alias after AS")
		}
		t.alias = p.next().text
	} else if p.cur().kind == tkIdent && !isKeyword(p.cur().text) {
		t.alias = p.next().text
	}
	if t.alias == "" {
		t.alias = t.class
	}
	return t, nil
}

// ---- Expressions: OR < AND < NOT < comparison < primary ----

func (p *sqlParser) parseExpr() (sqlExpr, error) { return p.parseOr() }

func (p *sqlParser) parseOr() (sqlExpr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("OR") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = binExpr{op: "OR", l: l, r: r}
	}
	return l, nil
}

func (p *sqlParser) parseAnd() (sqlExpr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKw("AND") {
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = binExpr{op: "AND", l: l, r: r}
	}
	return l, nil
}

func (p *sqlParser) parseNot() (sqlExpr, error) {
	if p.acceptKw("NOT") {
		e, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return notExpr{e: e}, nil
	}
	return p.parseCmp()
}

func (p *sqlParser) parseCmp() (sqlExpr, error) {
	l, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	// IS [NOT] NULL
	if p.acceptKw("IS") {
		neg := p.acceptKw("NOT")
		if err := p.expectKw("NULL"); err != nil {
			return nil, err
		}
		return isNullExpr{e: l, neg: neg}, nil
	}
	// [NOT] LIKE / IN
	neg := false
	if p.isKw("NOT") && (strings.EqualFold(p.toks[p.pos+1].text, "LIKE") || strings.EqualFold(p.toks[p.pos+1].text, "IN")) {
		p.pos++
		neg = true
	}
	if p.acceptKw("LIKE") {
		r, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return likeExpr{l: l, r: r, neg: neg}, nil
	}
	if p.acceptKw("IN") {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var list []sqlExpr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			list = append(list, e)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return inExpr{e: l, list: list, neg: neg}, nil
	}
	if neg {
		return nil, p.errf("expected LIKE or IN after NOT")
	}
	for _, op := range []string{"<=", ">=", "<>", "!=", "=", "<", ">"} {
		if p.acceptOp(op) {
			r, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			if op == "!=" {
				op = "<>"
			}
			return binExpr{op: op, l: l, r: r}, nil
		}
	}
	return l, nil
}

func (p *sqlParser) parsePrimary() (sqlExpr, error) {
	t := p.cur()
	switch {
	case t.kind == tkString:
		p.pos++
		return literal{val: t.text}, nil
	case t.kind == tkNumber:
		p.pos++
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, p.errf("invalid number %q", t.text)
		}
		return literal{val: f}, nil
	case p.isOp("("):
		p.pos++
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return e, nil
	case p.isKw("TRUE"):
		p.pos++
		return literal{val: true}, nil
	case p.isKw("FALSE"):
		p.pos++
		return literal{val: false}, nil
	case p.isKw("NULL"):
		p.pos++
		return literal{val: nil}, nil
	case p.isKw("CAST"):
		p.pos++
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("AS"); err != nil {
			return nil, err
		}
		if p.cur().kind != tkIdent {
			return nil, p.errf("expected type name in CAST")
		}
		typ := strings.ToUpper(p.next().text)
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return castExpr{e: e, typ: typ}, nil
	case t.kind == tkIdent && !isKeyword(t.text):
		p.pos++
		name := t.text
		// function call
		if p.isOp("(") {
			p.pos++
			var args []sqlExpr
			if !p.isOp(")") {
				for {
					a, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if !p.acceptOp(",") {
						break
					}
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return callExpr{fn: strings.ToUpper(name), args: args}, nil
		}
		// alias.field
		if p.acceptOp(".") {
			if p.cur().kind != tkIdent {
				return nil, p.errf("expected field name after %q.", name)
			}
			return colRef{alias: name, field: p.next().text}, nil
		}
		return colRef{field: name}, nil
	case p.isKw("COALESCE"):
		// COALESCE is in the keyword set (so it isn't taken as a table alias),
		// but syntactically it's a normal function call.
		p.pos++
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		var args []sqlExpr
		for {
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return callExpr{fn: "COALESCE", args: args}, nil
	}
	return nil, p.errf("unexpected token %q", t.text)
}
