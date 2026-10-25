package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/justinburger/mirage/internal/model"
)

const faultSelect = `SELECT data, created_at, updated_at FROM fault_rules`

func scanFaultRule(sc scanner) (model.FaultRule, error) {
	var (
		r                model.FaultRule
		data             []byte
		created, updated int64
	)
	if err := sc.Scan(&data, &created, &updated); err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("store: decode fault rule: %w", err)
	}
	r.CreatedAt, r.UpdatedAt = fromNanos(created), fromNanos(updated)
	return r, nil
}

// ListFaultRules returns every fault rule sorted by Order, then Name.
func (s *SQLite) ListFaultRules(ctx context.Context) ([]model.FaultRule, error) {
	rows, err := s.rdb.QueryContext(ctx, faultSelect+` ORDER BY rule_order, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list fault rules: %w", err)
	}
	defer rows.Close()
	out := []model.FaultRule{}
	for rows.Next() {
		r, err := scanFaultRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetFaultRule returns the fault rule with the given ID.
func (s *SQLite) GetFaultRule(ctx context.Context, id string) (model.FaultRule, error) {
	r, err := scanFaultRule(s.rdb.QueryRowContext(ctx, faultSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.FaultRule{}, notFound("fault rule", id)
	}
	return r, err
}

// PutFaultRule inserts or updates a fault rule by ID (assigning "flt_..."
// when empty).
func (s *SQLite) PutFaultRule(ctx context.Context, r model.FaultRule) (model.FaultRule, error) {
	if r.ID == "" {
		r.ID = model.NewID("flt")
	}
	now := s.nowUTC()
	r.CreatedAt, r.UpdatedAt = now, now
	data, err := json.Marshal(r)
	if err != nil {
		return model.FaultRule{}, fmt.Errorf("store: encode fault rule: %w", err)
	}
	var created int64
	err = s.wdb.QueryRowContext(ctx, `
		INSERT INTO fault_rules (id, name, rule_order, enabled, service_id, created_at, updated_at, data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, rule_order = excluded.rule_order, enabled = excluded.enabled,
			service_id = excluded.service_id, updated_at = excluded.updated_at, data = excluded.data
		RETURNING created_at`,
		r.ID, r.Name, r.Order, b2i(r.Enabled), r.ServiceID, nanos(now), nanos(now), string(data)).Scan(&created)
	if err != nil {
		return model.FaultRule{}, fmt.Errorf("store: put fault rule: %w", err)
	}
	r.CreatedAt = fromNanos(created)
	return r, nil
}

// DeleteFaultRule removes a fault rule.
func (s *SQLite) DeleteFaultRule(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "fault_rules", "fault rule", id)
}
