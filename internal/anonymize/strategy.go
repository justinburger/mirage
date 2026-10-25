package anonymize

import (
	"strings"
	"unicode/utf8"

	"github.com/justinburger/mirage/internal/model"
)

// defaultReplacement is what the redact strategy writes when a rule does not
// set its own Replacement.
const defaultReplacement = "[REDACTED]"

// action is a compiled strategy: what to do to a value once something decided
// to transform it.
type action struct {
	strategy    model.Strategy
	fake        model.FakeKind
	keepPrefix  int
	keepSuffix  int
	replacement string
}

// remove reports whether the action deletes its target. Contexts that cannot
// delete (a regex match inside text, a path segment, a detector hit) treat it
// as redact, which apply does by default.
func (a *action) remove() bool { return a.strategy == model.StrategyRemove }

// apply transforms v. An empty value is returned unchanged: there is nothing
// in it to hide, and a hash of "" would only add noise.
func (k *keyer) apply(a *action, v string) string {
	if v == "" {
		return v
	}
	switch a.strategy {
	case model.StrategyHash:
		return k.hashToken(v)
	case model.StrategyFake:
		return k.fake(a.fake, v)
	case model.StrategyMask:
		return maskValue(v, a.keepPrefix, a.keepSuffix)
	default: // redact, and remove where deleting is not possible
		return a.replacement
	}
}

// maskValue keeps keepPrefix leading and keepSuffix trailing characters and
// replaces everything else with '*', one '*' per character. When the kept
// parts would cover the whole value the value is masked completely, so a short
// secret is never revealed by a generous keep setting.
func maskValue(v string, keepPrefix, keepSuffix int) string {
	n := utf8.RuneCountInString(v)
	if keepPrefix < 0 {
		keepPrefix = 0
	}
	if keepSuffix < 0 {
		keepSuffix = 0
	}
	if keepPrefix+keepSuffix >= n {
		keepPrefix, keepSuffix = 0, 0
	}
	if n == len(v) { // ASCII fast path
		b := make([]byte, n)
		for i := range b {
			if i < keepPrefix || i >= n-keepSuffix {
				b[i] = v[i]
			} else {
				b[i] = '*'
			}
		}
		return string(b)
	}
	var sb strings.Builder
	i := 0
	for _, r := range v {
		if i < keepPrefix || i >= n-keepSuffix {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('*')
		}
		i++
	}
	return sb.String()
}
