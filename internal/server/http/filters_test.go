package http

import (
	"net/url"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

func TestLikeToRegex(t *testing.T) {
	cases := []struct {
		pat, s string
		ci     bool
		want   bool
	}{
		{"%nio%ivo%", "Antonio Nascivo", false, true}, // "nio" then "ivo" in order
		{"%nio%ivo%", "Antonio Ivo", false, false},    // "Ivo" capitalized -> lowercase "ivo" absent
		{"%nio%ivo%", "Antonio Ivo", true, true},      // ci makes "Ivo" match "ivo"
		{"%nio%ivo%", "Ivo Antonio", false, false},    // wrong order
		{"Rua %", "Rua das Flores", false, true},
		{"Rua %", "Avenida Brasil", false, false},
		{"S_o Paulo", "Sao Paulo", false, true}, // _ = single char
		{"S_o Paulo", "Sxo Paulo", false, true},
		{"S_o Paulo", "Soo Paulo", false, true},
		{"S_o Paulo", "So Paulo", false, false},   // _ needs exactly one char
		{"%SP%", "cidade sp litoral", true, true}, // case-insensitive
		{"%SP%", "cidade sp litoral", false, false},
		{"exato", "exato", false, true},
		{"exato", "exatox", false, false},
	}
	for _, c := range cases {
		re, err := likeToRegex(c.pat, c.ci)
		if err != nil {
			t.Fatalf("likeToRegex(%q): %v", c.pat, err)
		}
		if got := re.MatchString(c.s); got != c.want {
			t.Errorf("LIKE %q (ci=%v) vs %q = %v, want %v", c.pat, c.ci, c.s, got, c.want)
		}
	}
}

func TestMatcherEqAndLike(t *testing.T) {
	stored, _ := msgpack.Marshal(map[string]any{"nome": "Antonio Nascivo", "uf": "SP", "num": 100.0})

	// Build url.Values directly so raw % in LIKE patterns is not misread as a
	// percent-escape (real HTTP clients send %25, as the HTTP test does).
	mustMatch := func(want bool, pairs ...[2]string) {
		t.Helper()
		q := url.Values{}
		for _, p := range pairs {
			q.Set(p[0], p[1])
		}
		m, err := parseFilters(q)
		if err != nil {
			t.Fatalf("parseFilters(%v): %v", pairs, err)
		}
		if m == nil {
			t.Fatalf("parseFilters(%v) returned nil", pairs)
		}
		got, err := m.match(stored)
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		if got != want {
			t.Errorf("query %v = %v, want %v", pairs, got, want)
		}
	}

	mustMatch(true, [2]string{"eq.uf", "SP"})
	mustMatch(false, [2]string{"eq.uf", "RJ"})
	mustMatch(true, [2]string{"eq.uf", "sp"})                            // case-insensitive by DEFAULT
	mustMatch(false, [2]string{"eq.uf", "sp"}, [2]string{"ci", "false"}) // opt into case-sensitive
	mustMatch(true, [2]string{"eq.num", "100"})                          // numeric field compared as string
	mustMatch(true, [2]string{"like.nome", "%nio%ivo%"})
	mustMatch(true, [2]string{"like.nome", "%NIO%IVO%"}) // ci default: uppercase pattern matches
	mustMatch(false, [2]string{"like.nome", "%xyz%"})
	mustMatch(true, [2]string{"eq.uf", "SP"}, [2]string{"like.nome", "%nascivo"})  // AND of both (ci)
	mustMatch(false, [2]string{"eq.uf", "RJ"}, [2]string{"like.nome", "%Nascivo"}) // AND fails on uf
	mustMatch(false, [2]string{"eq.ausente", "x"})                                 // missing field never matches

	// No filters -> nil matcher.
	q, _ := url.ParseQuery("limit=10")
	if m, _ := parseFilters(q); m != nil {
		t.Errorf("expected nil matcher when no filters present")
	}
}
