// Package config loads Mirage's optional declarative configuration file
// (YAML or JSON) and applies it to the store at start-up.
//
// CONTRACT (implemented by the tooling agent):
//
//   - Load reads YAML (.yaml/.yml) or JSON (.json; anything else is tried as
//     YAML). YAML is decoded with gopkg.in/yaml.v3 into generic values,
//     converted to JSON and decoded with encoding/json (DisallowUnknownFields)
//     so the JSON tags on model types are the only schema (durations accept
//     "250ms" strings or millisecond numbers). ${VAR} and ${VAR:-default}
//     references in the raw file are expanded from the environment before
//     parsing. Errors name the file and, when possible, the offending field.
//   - Server fields fill defaults for CLI flags; flags given explicitly on
//     the command line win (the cmd package handles precedence).
//   - Apply is idempotent and matches existing objects by Name (services,
//     profiles, fault rules): an existing object keeps its ID and CreatedAt
//     and is overwritten with the file's definition; new ones are created.
//     Objects not in the file are left alone. Profiles keep their stored
//     secret unless the file sets one.
//   - Recordings: each entry ensures a recording with that name exists,
//     importing the HAR file at path HAR (relative to the config file's
//     directory) when it has to create it.
//   - A service's Recording field (a recording name) resolves to
//     RecordingID, creating an empty recording with that name if missing.
//     Profile (a profile name) resolves to ProfileID the same way, but must
//     exist (in the store or earlier in the file).
//   - Every object is validated (WithDefaults + Validate) before anything is
//     written; on a validation error nothing is applied.
package config

import (
	"context"

	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
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
}

// Server holds process settings (CLI flag defaults).
type Server struct {
	Admin      string `json:"admin,omitempty"`       // admin UI/API listen address
	Data       string `json:"data,omitempty"`        // SQLite database path
	AdminToken string `json:"admin_token,omitempty"` // bearer token for the admin API
	LogLevel   string `json:"log_level,omitempty"`   // debug, info, warn, error
	LogFormat  string `json:"log_format,omitempty"`  // text or json
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
}

// Load reads and parses a configuration file.
func Load(path string) (*Config, error) { panic("config: not implemented") }

// Apply writes the configuration's objects to the store.
func (c *Config) Apply(ctx context.Context, st store.Store) (ApplyResult, error) {
	panic("config: not implemented")
}
