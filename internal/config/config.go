// Package config loads Mirage's optional declarative configuration file
// (YAML or JSON) and applies it to the store at start-up.
//
// # File format
//
// A file has five optional top-level sections, each documented by the Go type
// it decodes into: server (process settings, the defaults for CLI flags),
// recordings, profiles (anonymization profiles), services and faults (fault
// rules). The JSON tags on the model types are the only schema, so the
// objects look exactly like the bodies of the management API (docs/API.md);
// durations are "250ms" strings or millisecond numbers. See
// examples/mirage.yaml for an annotated file.
//
// # Loading
//
//   - Load reads YAML (.yaml/.yml) or JSON (.json; any other extension is
//     tried as YAML). Parse does the same for bytes.
//   - ${VAR} and ${VAR:-default} references in the raw file are expanded
//     from the environment before parsing ($${ is a literal "${", and YAML
//     comment lines are not expanded). A variable that is not set and has no
//     default is an error.
//   - The document is checked against the Go types before it is decoded, so
//     problems are reported together, each with its line and field path:
//     unknown fields (with a "did you mean" hint), wrong types, bad durations,
//     duplicate keys. YAML anchors, aliases and merge keys work; plain YAML
//     numbers and booleans may fill string fields (Retry-After: 1).
//   - The decoded file is validated (Config.Validate) so Load fails early.
//     Errors are *Error values naming the file; they wrap model.ErrValidation.
//
// # Applying
//
// Apply writes the objects to the store. It matches existing objects by name
// (services, profiles, fault rules and recordings), keeps their IDs and
// creation times, leaves objects that are not in the file alone, and is
// idempotent: applying the same file twice changes nothing the second time
// and reports every object as unchanged. Names are resolved to IDs (a
// service's recording and profile, a fault rule's service). Everything is
// validated before the first write, so a bad file changes nothing. Apply does
// not modify the Config and is not atomic against concurrent writers of the
// same store; the file is a start-up concern.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

// Config is the configuration file.
type Config struct {
	Server     Server                       `json:"server"`
	Recordings []RecordingConfig            `json:"recordings,omitempty"`
	Profiles   []model.AnonymizationProfile `json:"profiles,omitempty"`
	Services   []ServiceConfig              `json:"services,omitempty"`
	Faults     []model.FaultRule            `json:"faults,omitempty"`

	// Dir is the directory of the loaded file (for relative paths).
	Dir string `json:"-"`

	// importHAR replaces the HAR importer in tests; nil means ops.ImportHAR.
	importHAR harImporter
}

// Server holds process settings (CLI flag defaults).
type Server struct {
	Admin      string `json:"admin,omitempty"`       // admin UI/API listen address
	Data       string `json:"data,omitempty"`        // SQLite database path
	AdminToken string `json:"admin_token,omitempty"` // bearer token for the admin API
	LogLevel   string `json:"log_level,omitempty"`   // debug, info, warn, error
	LogFormat  string `json:"log_format,omitempty"`  // text or json
	// AllowedHosts are DNS names the admin API may be reached by when it has
	// no admin token (IP addresses and localhost always work).
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

// RecordingConfig ensures a recording exists.
type RecordingConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	HAR         string `json:"har,omitempty"` // HAR file imported when the recording is created
}

// ServiceConfig is a service plus name references.
type ServiceConfig struct {
	model.Service
	// Recording names the recording (resolved to RecordingID).
	Recording string `json:"recording,omitempty"`
	// Profile names the anonymization profile (resolved to ProfileID).
	Profile string `json:"profile,omitempty"`
}

// ApplyResult reports what Apply changed.
type ApplyResult struct {
	Created []string `json:"created"` // "service web", "profile strict", …
	Updated []string `json:"updated"`
	// Unchanged lists objects that already matched the file. They are not
	// written, so their UpdatedAt does not move.
	Unchanged []string `json:"unchanged,omitempty"`
}

// Error is returned by Load and Parse when the file cannot be read as a
// valid configuration. It lists every problem found, each with its line and
// field path where known, and wraps model.ErrValidation.
type Error struct {
	File     string    `json:"file"`
	Problems []Problem `json:"problems"`
}

func (e *Error) Error() string {
	file := e.File
	if file == "" {
		file = "input"
	}
	if len(e.Problems) == 1 {
		return fmt.Sprintf("config: %s: %s", file, e.Problems[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %s: %d problems", file, len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.String())
	}
	return b.String()
}

// Unwrap returns model.ErrValidation.
func (e *Error) Unwrap() error { return model.ErrValidation }

// Load reads and parses a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	c, err := parse(path, data, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	c.Dir = dir
	return c, nil
}

// Parse decodes configuration bytes. name picks the format by extension
// (".json" is JSON, anything else YAML) and is used in error messages. Dir is
// left empty, so relative HAR paths resolve against the working directory.
func Parse(name string, data []byte) (*Config, error) {
	return parse(name, data, os.LookupEnv)
}

func parse(name string, data []byte, lookup func(string) (string, bool)) (*Config, error) {
	fail := func(ps ...Problem) (*Config, error) { return nil, &Error{File: name, Problems: ps} }
	isJSON := strings.EqualFold(filepath.Ext(name), ".json")

	data, err := expandEnv(data, !isJSON, lookup)
	if err != nil {
		var ee *envError
		if errors.As(err, &ee) {
			return fail(Problem{Line: ee.line, Msg: ee.msg})
		}
		return fail(Problem{Msg: err.Error()})
	}

	var root *node
	if isJSON {
		root, err = parseJSON(data)
	} else {
		root, err = parseYAML(data)
	}
	if err != nil {
		var pe *parseError
		if errors.As(err, &pe) {
			return fail(Problem{Line: pe.line, Msg: pe.msg})
		}
		return fail(Problem{Msg: err.Error()})
	}
	if root.kind == kindNull {
		root.kind = kindObject
	}
	if root.kind != kindObject {
		return fail(Problem{Line: root.line, Msg: fmt.Sprintf("the top level must be an object with sections such as services, got %s", root.kind)})
	}

	var ck checker
	ck.check(root, reflect.TypeFor[Config](), "")
	if len(ck.problems) > 0 {
		return fail(ck.problems...)
	}

	// The tree matches the types exactly, so this decode cannot fail on
	// content; DisallowUnknownFields is a second line of defence.
	dec := json.NewDecoder(bytes.NewReader(root.appendJSON(nil)))
	dec.DisallowUnknownFields()
	c := new(Config)
	if err := dec.Decode(c); err != nil {
		return fail(Problem{Msg: err.Error()})
	}

	if err := c.Validate(); err != nil {
		var ve *model.ValidationError
		if errors.As(err, &ve) {
			ps := make([]Problem, len(ve.Problems))
			for i, p := range ve.Problems {
				ps[i] = Problem{Msg: p}
			}
			return fail(ps...)
		}
		return fail(Problem{Msg: err.Error()})
	}
	return c, nil
}

// Placeholder IDs stand in for recordings and profiles that Apply resolves
// (or creates) by name, so objects can be validated before anything exists.
const (
	pendingRecordingID = "rec_pending"
	pendingProfileID   = "prof_pending"
)

// Validate checks the configuration without touching a store: server
// settings, then every object with WithDefaults + Validate (references by
// name count as resolved), and duplicate names or listen addresses within
// the file. It returns a *model.ValidationError listing every problem.
// References that need the store (a profile named but defined nowhere) are
// checked by Apply.
func (c *Config) Validate() error {
	var p []string
	addf := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	s := c.Server
	if s.Admin != "" {
		if _, _, err := net.SplitHostPort(s.Admin); err != nil {
			addf("server.admin %q must be host:port or :port", s.Admin)
		}
	}
	switch s.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		addf("server.log_level %q must be debug, info, warn or error", s.LogLevel)
	}
	switch s.LogFormat {
	case "", "text", "json":
	default:
		addf("server.log_format %q must be text or json", s.LogFormat)
	}

	seen := map[string]int{}
	for i, r := range c.Recordings {
		label := fmt.Sprintf("recordings[%d] %q", i, r.Name)
		if strings.TrimSpace(r.Name) == "" {
			addf("recordings[%d]: name is required", i)
			continue
		}
		if j, dup := seen[r.Name]; dup {
			addf("%s: duplicate name (also recordings[%d])", label, j)
		}
		seen[r.Name] = i
	}

	seen = map[string]int{}
	for i, pr := range c.Profiles {
		label := fmt.Sprintf("profiles[%d] %q", i, pr.Name)
		for _, msg := range problemsOf(withDefaultsProfile(pr).Validate()) {
			addf("%s: %s", label, msg)
		}
		key := normName(pr.Name)
		if key == "" {
			continue
		}
		if j, dup := seen[key]; dup {
			addf("%s: duplicate name (also profiles[%d]; profile names ignore case)", label, j)
		}
		seen[key] = i
	}

	seen = map[string]int{}
	listen := map[string]int{}
	for i, sc := range c.Services {
		label := fmt.Sprintf("services[%d] %q", i, sc.Name)
		svc := sc.forValidation()
		for _, msg := range problemsOf(svc.Validate()) {
			addf("%s: %s", label, msg)
		}
		if sc.Recording != "" && strings.TrimSpace(sc.Recording) == "" {
			addf("%s: recording name must not be blank", label)
		}
		if sc.Profile != "" && strings.TrimSpace(sc.Profile) == "" {
			addf("%s: profile name must not be blank", label)
		}
		if sc.Name != "" {
			if j, dup := seen[sc.Name]; dup {
				addf("%s: duplicate name (also services[%d])", label, j)
			}
			seen[sc.Name] = i
		}
		if key, ok := listenKey(sc.ListenAddr); ok {
			if j, dup := listen[key]; dup {
				addf("%s: listen_addr %q is also used by services[%d]", label, sc.ListenAddr, j)
			}
			listen[key] = i
		}
	}

	seen = map[string]int{}
	for i, f := range c.Faults {
		label := fmt.Sprintf("faults[%d] %q", i, f.Name)
		for _, msg := range problemsOf(f.WithDefaults().Validate()) {
			addf("%s: %s", label, msg)
		}
		name := strings.TrimSpace(f.Name)
		if name == "" {
			continue
		}
		if j, dup := seen[name]; dup {
			addf("%s: duplicate name (also faults[%d])", label, j)
		}
		seen[name] = i
	}

	if len(p) == 0 {
		return nil
	}
	return &model.ValidationError{Problems: p}
}

// forValidation returns the service with defaults applied and name
// references replaced by placeholder IDs.
func (sc ServiceConfig) forValidation() model.Service {
	s := sc.Service
	if sc.Recording != "" {
		s.RecordingID = pendingRecordingID
	}
	if sc.Profile != "" {
		s.ProfileID = pendingProfileID
	}
	return s.WithDefaults()
}

// withDefaultsProfile applies WithDefaults to a copy that shares no rule
// storage with p (WithDefaults edits rules in place).
func withDefaultsProfile(p model.AnonymizationProfile) model.AnonymizationProfile {
	p.Rules = append([]model.AnonymizationRule(nil), p.Rules...)
	return p.WithDefaults()
}

// problemsOf lists the problems of a validation error (or the error text).
func problemsOf(err error) []string {
	if err == nil {
		return nil
	}
	var ve *model.ValidationError
	if errors.As(err, &ve) {
		return ve.Problems
	}
	return []string{err.Error()}
}

// normName is the comparison key of profile names: the store treats them as
// equal ignoring case and surrounding white space.
func normName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// listenKey normalises a listen address the way the store does when it
// enforces uniqueness: wildcard hosts are one socket, port 0 never clashes.
func listenKey(addr string) (key string, checked bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", false // Validate reports malformed addresses
	}
	if port == "0" {
		return "", false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = ""
	default:
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, port), true
}
