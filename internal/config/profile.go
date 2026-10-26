package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

// LoadProfile reads an anonymization profile from a YAML or JSON file, for
// tools that anonymize without a database (mirage anonymize --profile-file).
//
// The file is either one profile object (the fields of the "profiles" entries
// of a configuration file, see docs/API.md) or a complete configuration file
// with exactly one entry under "profiles". Unknown fields and wrong types are
// errors that name the field and line, exactly as for Load. ${VAR}
// references are expanded as in configuration files.
//
// A profile without a name is called after the file. The secret is left
// empty when the file has none; the caller decides whether to generate one.
func LoadProfile(path string) (model.AnonymizationProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.AnonymizationProfile{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	p, err := parseProfile(path, data, os.LookupEnv)
	if err != nil {
		return model.AnonymizationProfile{}, err
	}
	if strings.TrimSpace(p.Name) == "" {
		base := filepath.Base(path)
		p.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	return p, nil
}

func parseProfile(name string, data []byte, lookup func(string) (string, bool)) (model.AnonymizationProfile, error) {
	fail := func(ps ...Problem) (model.AnonymizationProfile, error) {
		return model.AnonymizationProfile{}, &Error{File: name, Problems: ps}
	}
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
	if root.kind != kindObject {
		return fail(Problem{Line: root.line, Msg: fmt.Sprintf("a profile file must hold an object, got %s", root.kind)})
	}

	// A configuration file with one profile is accepted as well.
	if list, ok := root.get("profiles"); ok {
		if list.kind != kindArray || len(list.items) != 1 {
			return fail(Problem{Line: list.line, Field: "profiles", Msg: "expected exactly one profile in the list; pass a file with a single profile"})
		}
		root = list.items[0]
	}

	var ck checker
	ck.check(root, reflect.TypeFor[model.AnonymizationProfile](), "")
	if len(ck.problems) > 0 {
		return fail(ck.problems...)
	}
	dec := json.NewDecoder(bytes.NewReader(root.appendJSON(nil)))
	dec.DisallowUnknownFields()
	var p model.AnonymizationProfile
	if err := dec.Decode(&p); err != nil {
		return fail(Problem{Msg: err.Error()})
	}
	p.Rules = append([]model.AnonymizationRule(nil), p.Rules...)
	p = p.WithDefaults()
	if strings.TrimSpace(p.Name) != "" {
		if err := p.Validate(); err != nil {
			ps := problemsOf(err)
			out := make([]Problem, len(ps))
			for i, m := range ps {
				out[i] = Problem{Msg: m}
			}
			return fail(out...)
		}
	}
	return p, nil
}
