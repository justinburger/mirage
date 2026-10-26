package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/justinburger/mirage/internal/model"
)

const profileSelect = `SELECT data, secret, created_at, updated_at FROM profiles`

func scanProfile(sc scanner) (model.AnonymizationProfile, error) {
	var (
		p                model.AnonymizationProfile
		data             []byte
		secret           string
		created, updated int64
	)
	if err := sc.Scan(&data, &secret, &created, &updated); err != nil {
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("store: decode profile: %w", err)
	}
	p.Secret, p.HasSecret = secret, secret != ""
	p.CreatedAt, p.UpdatedAt = fromNanos(created), fromNanos(updated)
	return p, nil
}

// defaultProfileRevKey is the settings key holding the model.DefaultProfileRev
// the stored built-in profile was last brought up to.
const defaultProfileRevKey = "default_profile_rev"

// seedDefaultProfile inserts the built-in profile, with a fresh secret, unless
// a profile with that ID already exists, and then upgrades a profile written
// by an older release (see upgradeDefaultProfile). It is safe to race with
// another process opening the same database.
func (s *SQLite) seedDefaultProfile(ctx context.Context) error {
	if err := s.insertDefaultProfile(ctx); err != nil {
		return err
	}
	return s.upgradeDefaultProfile(ctx)
}

// upgradeDefaultProfile adds the rules and redacted headers that this release
// built into the default profile to a database created by an older one. The
// seed never overwrites an existing profile, so without this a database would
// keep the rules of the release that created it for ever and stay exposed to
// everything later releases learned to catch. Only what is missing is added
// (rules by ID, headers ignoring case): rules the user edited, switched off
// or added are kept, and the profile's secret and name are not touched. The
// revision reached is recorded, so a rule the user deletes afterwards stays
// deleted; a database already at (or beyond) this release's revision is left
// alone.
func (s *SQLite) upgradeDefaultProfile(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		rev := 1 // databases from before the revision was recorded
		var v string
		switch err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, defaultProfileRevKey).Scan(&v); {
		case err == nil:
			if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
				rev = n
			}
		case errors.Is(err, sql.ErrNoRows):
		default:
			return fmt.Errorf("store: read %s: %w", defaultProfileRevKey, err)
		}
		if rev >= model.DefaultProfileRev {
			return nil
		}
		var data []byte
		switch err := tx.QueryRowContext(ctx, `SELECT data FROM profiles WHERE id = ?`, model.DefaultProfileID).Scan(&data); {
		case errors.Is(err, sql.ErrNoRows):
			// Not there (the seed lost a race with a delete): nothing to upgrade.
			return nil
		case err != nil:
			return fmt.Errorf("store: read default profile: %w", err)
		}
		var p model.AnonymizationProfile
		if err := json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("store: decode default profile: %w", err)
		}
		up, changed := model.UpgradeDefaultProfile(p, rev)
		if changed {
			enc, err := json.Marshal(up.WithDefaults())
			if err != nil {
				return fmt.Errorf("store: encode default profile: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE profiles SET data = ?, updated_at = ? WHERE id = ?`,
				string(enc), nanos(s.nowUTC()), model.DefaultProfileID); err != nil {
				return fmt.Errorf("store: upgrade default profile: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			defaultProfileRevKey, strconv.Itoa(model.DefaultProfileRev), nanos(s.nowUTC())); err != nil {
			return fmt.Errorf("store: record %s: %w", defaultProfileRevKey, err)
		}
		return nil
	})
}

func (s *SQLite) insertDefaultProfile(ctx context.Context) error {
	p := model.DefaultAnonymizationProfile().WithDefaults()
	now := nanos(s.nowUTC())
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("store: encode default profile: %w", err)
	}
	if _, err := s.wdb.ExecContext(ctx, `
		INSERT INTO profiles (id, name, secret, created_at, updated_at, data)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		p.ID, p.Name, model.NewSecret(), now, now, string(data)); err != nil {
		return fmt.Errorf("store: seed default profile: %w", err)
	}
	return nil
}

// ListProfiles returns every profile: the built-in "default" first, then the
// rest by name. Secrets are included.
func (s *SQLite) ListProfiles(ctx context.Context) ([]model.AnonymizationProfile, error) {
	rows, err := s.rdb.QueryContext(ctx, profileSelect+
		` ORDER BY (id = '`+model.DefaultProfileID+`') DESC, name COLLATE NOCASE, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list profiles: %w", err)
	}
	defer rows.Close()
	out := []model.AnonymizationProfile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProfile returns the profile with the given ID, including its secret.
func (s *SQLite) GetProfile(ctx context.Context, id string) (model.AnonymizationProfile, error) {
	p, err := scanProfile(s.rdb.QueryRowContext(ctx, profileSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.AnonymizationProfile{}, notFound("profile", id)
	}
	return p, err
}

// PutProfile inserts or updates a profile by ID (assigning "prof_..." when
// empty). An empty Secret keeps the stored secret on update and generates a
// fresh one on insert, so every profile can key deterministic anonymization;
// a non-empty Secret replaces the stored one.
//
// Every rule with an empty ID gets a "rule_..." ID (existing IDs are kept, so
// a profile read back and saved again keeps its rule IDs). Profile names are
// unique ignoring case and surrounding white space: a name used by another
// profile returns an error wrapping model.ErrConflict.
func (s *SQLite) PutProfile(ctx context.Context, p model.AnonymizationProfile) (model.AnonymizationProfile, error) {
	if p.ID == "" {
		p.ID = model.NewID("prof")
	}
	p.Rules = withRuleIDs(p.Rules)
	keepStored := p.Secret == ""
	secret := p.Secret
	if keepStored {
		secret = model.NewSecret() // used only when the profile is new
	}
	now := s.nowUTC()
	p.CreatedAt, p.UpdatedAt = now, now
	// The secret lives in its own column; the JSON never carries it.
	enc := p
	enc.Secret, enc.HasSecret = "", false
	data, err := json.Marshal(enc)
	if err != nil {
		return model.AnonymizationProfile{}, fmt.Errorf("store: encode profile: %w", err)
	}
	var created int64
	err = s.write(ctx, func(tx *sql.Tx) error {
		// The writer is exclusive for the whole transaction, so the name check
		// and the upsert cannot interleave with another PutProfile. Profiles
		// are few, which lets Go do the Unicode-aware comparison.
		if err := checkProfileName(ctx, tx, p.ID, p.Name); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `
			INSERT INTO profiles (id, name, secret, created_at, updated_at, data)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				secret = CASE WHEN ? THEN profiles.secret ELSE excluded.secret END,
				updated_at = excluded.updated_at, data = excluded.data
			RETURNING secret, created_at`,
			p.ID, p.Name, secret, nanos(now), nanos(now), string(data), b2i(keepStored)).Scan(&secret, &created)
		if err != nil {
			return fmt.Errorf("store: put profile: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.AnonymizationProfile{}, err
	}
	p.Secret, p.HasSecret = secret, secret != ""
	p.CreatedAt = fromNanos(created)
	return p, nil
}

// withRuleIDs returns rules with every empty ID filled in. The caller's slice
// is never modified.
func withRuleIDs(rules []model.AnonymizationRule) []model.AnonymizationRule {
	if !slices.ContainsFunc(rules, func(r model.AnonymizationRule) bool { return r.ID == "" }) {
		return rules
	}
	out := slices.Clone(rules)
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = model.NewID("rule")
		}
	}
	return out
}

// sameProfileName compares profile names ignoring case and surrounding white
// space.
func sameProfileName(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// checkProfileName reports a conflict when a profile other than id already
// uses name.
func checkProfileName(ctx context.Context, tx *sql.Tx, id, name string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM profiles WHERE id <> ?`, id)
	if err != nil {
		return fmt.Errorf("store: put profile: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			return fmt.Errorf("store: put profile: %w", err)
		}
		if sameProfileName(other, name) {
			return conflict("profile name %q is already in use", name)
		}
	}
	return rows.Err()
}

// DeleteProfile removes a profile. The built-in "default" profile cannot be
// deleted (the error wraps model.ErrConflict).
func (s *SQLite) DeleteProfile(ctx context.Context, id string) error {
	if id == model.DefaultProfileID {
		return conflict("the built-in %q profile cannot be deleted", id)
	}
	return s.deleteByID(ctx, "profiles", "profile", id)
}
