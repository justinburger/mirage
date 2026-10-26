//go:build race

package replay

// raceEnabled is true when the tests run under the race detector, which slows
// the request path down several times over.
const raceEnabled = true
