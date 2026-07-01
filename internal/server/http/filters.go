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
//	eq.<field>=<value>    exact match on a field (string comparison)
//	like.<field>=<pat>    SQL LIKE: % = any sequence, _ = any single char
//	ci=false              opt into case-SENSITIVE matching
//
// Matching is case-insensitive BY DEFAULT (natural for text search); pass
// ci=false for case-sensitive. Multiple filters combine with AND. There is no
// secondary index, so matching decodes every object in the class (a full
// scan) — see QueryObjects.

type filter struct {
	field string
	eq    string         // equality target (lowercased when ci); used when re == nil
	re    *regexp.Regexp // compiled LIKE matcher; used when non-nil
}

type matcher struct {
	filters []filter
	ci      bool
}

// parseFilters builds a matcher from the query params, or nil if there are no
// eq./like. filters. It errors only on an invalid LIKE pattern.
func parseFilters(q url.Values) (*matcher, error) {
	// Case-insensitive by default; ci=false / ci=0 opts into case-sensitive.
	ci := q.Get("ci") != "false" && q.Get("ci") != "0"
	var filters []filter
	for key, vals := range q {
		if len(vals) == 0 {
			continue
		}
		v := vals[0]
		switch {
		case strings.HasPrefix(key, "eq."):
			field := key[len("eq."):]
			if field == "" {
				continue
			}
			target := v
			if ci {
				target = strings.ToLower(target)
			}
			filters = append(filters, filter{field: field, eq: target})
		case strings.HasPrefix(key, "like."):
			field := key[len("like."):]
			if field == "" {
				continue
			}
			re, err := likeToRegex(v, ci)
			if err != nil {
				return nil, fmt.Errorf("invalid like pattern for %q: %w", field, err)
			}
			filters = append(filters, filter{field: field, re: re})
		}
	}
	if len(filters) == 0 {
		return nil, nil
	}
	return &matcher{filters: filters, ci: ci}, nil
}

// match decodes the stored object and reports whether it satisfies all filters.
func (m *matcher) match(stored []byte) (bool, error) {
	var obj map[string]any
	if err := msgpack.Unmarshal(stored, &obj); err != nil {
		return false, err
	}
	for _, f := range m.filters {
		raw, ok := obj[f.field]
		if !ok {
			return false, nil // missing field never matches
		}
		sval, ok := fieldString(raw)
		if !ok {
			return false, nil
		}
		if f.re != nil {
			if !f.re.MatchString(sval) {
				return false, nil
			}
			continue
		}
		cmp := sval
		if m.ci {
			cmp = strings.ToLower(cmp)
		}
		if cmp != f.eq {
			return false, nil
		}
	}
	return true, nil
}

// likeToRegex translates a SQL LIKE pattern into an anchored regexp.
func likeToRegex(pattern string, ci bool) (*regexp.Regexp, error) {
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
	flags := "(?s)" // let . span newlines
	if ci {
		flags = "(?is)"
	}
	return regexp.Compile(flags + b.String())
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
