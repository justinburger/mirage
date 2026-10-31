package pathnorm

import (
	"slices"
	"strings"
)

// AnonymizedEndpoint returns the endpoint to store with an exchange whose
// path was anonymized (rawPath before, anonPath after). The endpoint of the
// raw path is the stable grouping ("/users/{id}" whatever the id became), but
// its literal segments are raw values the anonymizer rewrote in the stored
// path ("/people/123-45-6789"). So the result takes the segments of the
// endpoint of the anonymized path, except where the raw path was templated: a
// placeholder carries no data and keeps the grouping stable. A template the
// user configured (a pattern of n) is kept as it is.
func (n *Normalizer) AnonymizedEndpoint(method, rawPath, anonPath string) string {
	rawEP := n.Endpoint(method, rawPath)
	anonEP := n.Endpoint(method, anonPath)
	if rawEP == anonEP {
		return rawEP
	}
	_, rawT, ok1 := strings.Cut(rawEP, " ")
	prefix, anonT, ok2 := strings.Cut(anonEP, " ")
	if !ok1 || !ok2 {
		return anonEP
	}
	if slices.Contains(n.Patterns(), rawT) {
		return prefix + " " + rawT
	}
	rs, as := strings.Split(rawT, "/"), strings.Split(anonT, "/")
	if len(rs) != len(as) {
		return anonEP
	}
	for i := range as {
		if isBuiltinPlaceholder(rs[i]) {
			as[i] = rs[i]
		}
	}
	return prefix + " " + strings.Join(as, "/")
}

// isBuiltinPlaceholder reports whether seg is one of the placeholders the
// normalizer substitutes for ids and other opaque values.
func isBuiltinPlaceholder(seg string) bool {
	switch seg {
	case "{id}", "{uuid}", "{hex}", "{token}", "{email}", "{date}":
		return true
	}
	return false
}
