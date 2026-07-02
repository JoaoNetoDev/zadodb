package http

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/vmihailenco/msgpack/v5"
)

// Query filters are expressed as URL params:
//
//	eq.<field>=<value>          exact match on a field (string comparison)
//	like.<field>=<pat>          SQL LIKE: % = any sequence, _ = any single char
//	eq.<rel>.<field>=<value>    match on a related class (see join.go)
//	like.<rel>.<field>=<pat>    LIKE on a related class
//	ci=false                    opt into case-SENSITIVE matching
//	ai=false                    opt into accent-SENSITIVE matching
//
// Matching is case- AND accent-insensitive BY DEFAULT (natural for pt-BR text
// search: "mossoro" matches "Mossoró"); pass ci=false / ai=false to opt out.
// Multiple filters combine with AND. There is no secondary index, so matching
// decodes every object in the class (a full scan) — see QueryObjects.

// foldOpts controls case/accent folding applied to both sides of a comparison.
type foldOpts struct {
	ci bool // case-insensitive (lowercase)
	ai bool // accent-insensitive (strip diacritics)
}

func parseFoldOpts(q url.Values) foldOpts {
	return foldOpts{
		ci: q.Get("ci") != "false" && q.Get("ci") != "0",
		ai: q.Get("ai") != "false" && q.Get("ai") != "0",
	}
}

// fold normalizes s for comparison per the fold options.
func (o foldOpts) fold(s string) string {
	if o.ai {
		s = stripAccents(s)
	}
	if o.ci {
		s = strings.ToLower(s)
	}
	return s
}

// accentTable maps common Latin diacritics (both cases) to their base letter.
// Covers Portuguese and neighboring Latin scripts; single-rune mappings keep it
// usable with strings.Map. Case is normalized separately by fold.
var accentTable = map[rune]rune{
	'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Ä': 'A', 'Å': 'A',
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'Ç': 'C', 'ç': 'c',
	'È': 'E', 'É': 'E', 'Ê': 'E', 'Ë': 'E',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'Ì': 'I', 'Í': 'I', 'Î': 'I', 'Ï': 'I',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'Ñ': 'N', 'ñ': 'n',
	'Ò': 'O', 'Ó': 'O', 'Ô': 'O', 'Õ': 'O', 'Ö': 'O',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'Ù': 'U', 'Ú': 'U', 'Û': 'U', 'Ü': 'U',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'Ý': 'Y', 'ý': 'y', 'ÿ': 'y',
}

func stripAccents(s string) string {
	return strings.Map(func(r rune) rune {
		if b, ok := accentTable[r]; ok {
			return b
		}
		return r
	}, s)
}

type filter struct {
	field string
	eq    string         // folded equality target; used when re == nil
	re    *regexp.Regexp // compiled LIKE matcher over folded text; used when non-nil
}

type matcher struct {
	filters []filter
	fold    foldOpts
}

// filterSpec is a single parsed eq./like. param, possibly scoped to a related
// class by an alias (the part before the dot in the field path).
type filterSpec struct {
	alias string // "" = the base class; otherwise a related class (join)
	field string
	like  bool
	value string
}

// parseFilterSpecs extracts all eq./like. filters from the query params. A
// dotted field path ("rel.field") scopes the filter to a related class.
func parseFilterSpecs(q url.Values) []filterSpec {
	var specs []filterSpec
	for key, vals := range q {
		if len(vals) == 0 {
			continue
		}
		var like bool
		var path string
		switch {
		case strings.HasPrefix(key, "eq."):
			path = key[len("eq."):]
		case strings.HasPrefix(key, "like."):
			path, like = key[len("like."):], true
		default:
			continue
		}
		if path == "" {
			continue
		}
		alias, field := "", path
		if i := strings.IndexByte(path, '.'); i >= 0 {
			alias, field = path[:i], path[i+1:]
		}
		if field == "" {
			continue
		}
		specs = append(specs, filterSpec{alias: alias, field: field, like: like, value: vals[0]})
	}
	return specs
}

// buildMatcher compiles specs (already scoped to one class) into a matcher.
func buildMatcher(specs []filterSpec, fo foldOpts) (*matcher, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	m := &matcher{fold: fo}
	for _, s := range specs {
		if s.like {
			re, err := likeToRegex(fo.fold(s.value))
			if err != nil {
				return nil, fmt.Errorf("invalid like pattern for %q: %w", s.field, err)
			}
			m.filters = append(m.filters, filter{field: s.field, re: re})
		} else {
			m.filters = append(m.filters, filter{field: s.field, eq: fo.fold(s.value)})
		}
	}
	return m, nil
}

// parseFilters builds a matcher for the base class from the query params, or nil
// if there are no base-level eq./like. filters. Relation filters (with a dotted
// path) are ignored here — join.go handles them.
func parseFilters(q url.Values) (*matcher, error) {
	fo := parseFoldOpts(q)
	var base []filterSpec
	for _, s := range parseFilterSpecs(q) {
		if s.alias == "" {
			base = append(base, s)
		}
	}
	return buildMatcher(base, fo)
}

// match decodes the stored object and reports whether it satisfies all filters.
func (m *matcher) match(stored []byte) (bool, error) {
	var obj map[string]any
	if err := msgpack.Unmarshal(stored, &obj); err != nil {
		return false, err
	}
	return m.matchMap(obj), nil
}

// matchMap reports whether an already-decoded object satisfies all filters.
func (m *matcher) matchMap(obj map[string]any) bool {
	for _, f := range m.filters {
		raw, ok := obj[f.field]
		if !ok {
			return false // missing field never matches
		}
		sval, ok := fieldString(raw)
		if !ok {
			return false
		}
		if f.re != nil {
			if !f.re.MatchString(m.fold.fold(sval)) {
				return false
			}
			continue
		}
		if m.fold.fold(sval) != f.eq {
			return false
		}
	}
	return true
}

// likeToRegex translates a SQL LIKE pattern into an anchored regexp. The pattern
// is expected to be pre-folded, and matching runs against pre-folded text, so no
// case flag is needed (only (?s) so . spans newlines).
func likeToRegex(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile("(?s)" + b.String())
}

// fieldString renders a decoded field value as a string for comparison.
func fieldString(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case int:
		return strconv.Itoa(t), true
	case uint64:
		return strconv.FormatUint(t, 10), true
	default:
		return fmt.Sprintf("%v", t), true
	}
}
