package http

import (
	"fmt"

	"github.com/JoaoNetoDev/zadodb/internal/storage"
	"github.com/vmihailenco/msgpack/v5"
)

// Joins are resolved without secondary indexes, as a chain of hash semi-joins
// driven by registered relationships (see storage.Relationship). For a query on
// a base class carrying relation filters like eq.uf.sigla=RN&like.municipio.nome=mossor%,
// the resolver:
//
//  1. finds a path base -> ... -> target for each filtered relation (BFS over
//     the relationships graph; an alias is the target class name);
//  2. resolves each class from the far end inward: it scans the class, keeps the
//     objects that satisfy that class's own filters AND the downstream
//     constraints, and collects the join-key values the parent points at;
//  3. yields a predicate on the base class: base.localField must be in the
//     allowed set of the related class (combined with the base's own filters).
//
// Cost is O(n) per involved class (a full scan each). Related classes are
// usually small; the base scan is the same one ListObjects already does.

// edge is a relationship anchored at its source class.
type edge struct {
	from string
	rel  storage.Relationship
}

// buildJoinPredicate returns a match predicate for the base class that enforces
// base's own filters plus all relation filters, or an error if a relation
// alias cannot be resolved.
func (s *Server) buildJoinPredicate(project, baseClass string, baseMatcher *matcher, relSpecs []filterSpec, fo foldOpts) (func([]byte) (bool, error), error) {
	byAlias := map[string][]filterSpec{}
	for _, sp := range relSpecs {
		byAlias[sp.alias] = append(byAlias[sp.alias], sp)
	}

	plan := &joinPlan{
		engine:     s.engine,
		project:    project,
		edgesFrom:  map[string][]edge{},
		filtersFor: map[string]*matcher{},
	}

	// For each alias, find a path from the base and register its edges + filters.
	for alias, specs := range byAlias {
		path, err := plan.pathTo(baseClass, alias)
		if err != nil {
			return nil, err
		}
		for _, e := range path {
			plan.addEdge(e)
		}
		m, err := buildMatcher(stripAlias(specs), fo)
		if err != nil {
			return nil, err
		}
		plan.filtersFor[alias] = m
	}

	// Resolve the allowed-value sets for the base class's outgoing edges.
	type baseConstraint struct {
		localField string
		allowed    map[string]struct{}
	}
	var constraints []baseConstraint
	for _, e := range plan.edgesFrom[baseClass] {
		allowed, err := plan.resolveAllowed(e.rel.ToClass, e.rel.RemoteField)
		if err != nil {
			return nil, err
		}
		constraints = append(constraints, baseConstraint{localField: e.rel.LocalField, allowed: allowed})
	}

	return func(stored []byte) (bool, error) {
		var obj map[string]any
		if err := msgpack.Unmarshal(stored, &obj); err != nil {
			return false, err
		}
		if baseMatcher != nil && !baseMatcher.matchMap(obj) {
			return false, nil
		}
		for _, c := range constraints {
			v, ok := fieldString(obj[c.localField])
			if !ok {
				return false, nil
			}
			if _, in := c.allowed[v]; !in {
				return false, nil
			}
		}
		return true, nil
	}, nil
}

// stripAlias returns copies of specs with the alias cleared, so they can be
// compiled against the related class's own fields.
func stripAlias(specs []filterSpec) []filterSpec {
	out := make([]filterSpec, len(specs))
	for i, s := range specs {
		s.alias = ""
		out[i] = s
	}
	return out
}

type joinPlan struct {
	engine     *storage.Engine
	project    string
	edgesFrom  map[string][]edge   // class -> outgoing edges used by the plan
	filtersFor map[string]*matcher // target class -> its own filters
}

func (p *joinPlan) addEdge(e edge) {
	for _, x := range p.edgesFrom[e.from] {
		if x.rel.Name == e.rel.Name {
			return // already present
		}
	}
	p.edgesFrom[e.from] = append(p.edgesFrom[e.from], e)
}

// pathTo finds a path of relationships from `base` to a target class (`alias`)
// via BFS over the relationships graph. An alias matches a relationship's target
// class or its name. Returns the ordered edges, or an error if unreachable.
func (p *joinPlan) pathTo(base, alias string) ([]edge, error) {
	if base == alias {
		return nil, nil
	}
	type node struct {
		class string
		path  []edge
	}
	visited := map[string]bool{base: true}
	queue := []node{{class: base}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, rel := range p.engine.ListRelationships(p.project, cur.class) {
			e := edge{from: cur.class, rel: rel}
			next := append(append([]edge(nil), cur.path...), e)
			if rel.ToClass == alias || rel.Name == alias {
				return next, nil
			}
			if !visited[rel.ToClass] {
				visited[rel.ToClass] = true
				queue = append(queue, node{class: rel.ToClass, path: next})
			}
		}
	}
	return nil, fmt.Errorf("unknown relation %q from class %q (register it via POST .../relationships)", alias, base)
}

// resolveAllowed scans `class` and returns the set of fieldString(wantField)
// values over objects that satisfy this class's own filters and all downstream
// constraints (recursively).
func (p *joinPlan) resolveAllowed(class, wantField string) (map[string]struct{}, error) {
	type childConstraint struct {
		localField string
		allowed    map[string]struct{}
	}
	var children []childConstraint
	for _, e := range p.edgesFrom[class] {
		allowed, err := p.resolveAllowed(e.rel.ToClass, e.rel.RemoteField)
		if err != nil {
			return nil, err
		}
		children = append(children, childConstraint{localField: e.rel.LocalField, allowed: allowed})
	}

	// Apply this class's own filters at scan time so only matching related rows
	// are materialized (the child constraints below still run per row).
	own := p.filtersFor[class]
	var ownMatch func([]byte) (bool, error)
	if own != nil {
		ownMatch = own.match
	}
	objs, err := p.engine.QueryObjects(p.project, class, ownMatch, 0, 0)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, o := range objs {
		var obj map[string]any
		if err := msgpack.Unmarshal(o.Data, &obj); err != nil {
			return nil, err
		}
		ok := true
		for _, c := range children {
			v, has := fieldString(obj[c.localField])
			if !has {
				ok = false
				break
			}
			if _, in := c.allowed[v]; !in {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if wv, has := fieldString(obj[wantField]); has {
			result[wv] = struct{}{}
		}
	}
	return result, nil
}
