package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/justinburger/mirage/internal/model"
)

// nowUTC returns the current time truncated to what the database stores
// (nanoseconds, UTC, no monotonic reading), so a value returned by a write
// equals the one a later read returns.
func (s *SQLite) nowUTC() time.Time { return time.Unix(0, s.now().UnixNano()).UTC() }

// deleteByID deletes one row of a table keyed by id and reports ErrNotFound
// when nothing matched. table is always a package constant, never user input.
func (s *SQLite) deleteByID(ctx context.Context, table, kind, id string) error {
	res, err := s.wdb.ExecContext(ctx, "DELETE FROM "+table+" WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: delete %s %q: %w", kind, id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return notFound(kind, id)
	}
	return nil
}

func scanService(sc scanner) (model.Service, error) {
	var (
		svc              model.Service
		data             []byte
		created, updated int64
	)
	if err := sc.Scan(&data, &created, &updated); err != nil {
		return svc, err
	}
	if err := json.Unmarshal(data, &svc); err != nil {
		return svc, fmt.Errorf("store: decode service: %w", err)
	}
	svc.CreatedAt, svc.UpdatedAt = fromNanos(created), fromNanos(updated)
	return svc, nil
}

// ListServices returns every service ordered by name.
func (s *SQLite) ListServices(ctx context.Context) ([]model.Service, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT data, created_at, updated_at FROM services ORDER BY name COLLATE NOCASE, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	defer rows.Close()
	out := []model.Service{}
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// GetService returns the service with the given ID.
func (s *SQLite) GetService(ctx context.Context, id string) (model.Service, error) {
	svc, err := scanService(s.rdb.QueryRowContext(ctx,
		`SELECT data, created_at, updated_at FROM services WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Service{}, notFound("service", id)
	}
	return svc, err
}

// PutService inserts or updates a service by ID (assigning "svc_..." when
// empty). A Name another service already uses, ignoring case and surrounding
// white space (like profile names), or a ListenAddr already used by another
// service, returns an error wrapping model.ErrConflict. A database that
// already holds two names differing only in case still opens; one of them has
// to be renamed before it can be saved again.
//
// Listen addresses are compared host-normalised (see listenKey): ":9001",
// "0.0.0.0:9001" and "[::]:9001" are the same socket. Port 0 means "pick a
// free port" and never conflicts, so any number of services may use ":0" or
// "127.0.0.1:0".
func (s *SQLite) PutService(ctx context.Context, svc model.Service) (model.Service, error) {
	if svc.ID == "" {
		svc.ID = model.NewID("svc")
	}
	now := s.nowUTC()
	svc.CreatedAt, svc.UpdatedAt = now, now
	data, err := json.Marshal(svc)
	if err != nil {
		return model.Service{}, fmt.Errorf("store: encode service: %w", err)
	}
	var created int64
	err = s.write(ctx, func(tx *sql.Tx) error {
		// The writer is exclusive for the whole transaction, so the address and
		// name checks and the upsert cannot interleave with another PutService.
		if err := checkListenAddr(ctx, tx, svc.ID, svc.ListenAddr); err != nil {
			return err
		}
		if err := checkServiceName(ctx, tx, svc.ID, svc.Name); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, `
			INSERT INTO services (id, name, listen_addr, mode, enabled, recording_id, created_at, updated_at, data)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name, listen_addr = excluded.listen_addr, mode = excluded.mode,
				enabled = excluded.enabled, recording_id = excluded.recording_id,
				updated_at = excluded.updated_at, data = excluded.data
			RETURNING created_at`,
			svc.ID, svc.Name, svc.ListenAddr, string(svc.Mode), b2i(svc.Enabled), svc.RecordingID,
			nanos(now), nanos(now), string(data)).Scan(&created)
		if isUniqueViolation(err) {
			// Only services.name is still UNIQUE; the id is handled by the upsert.
			return conflict("service name %q is already in use", svc.Name)
		}
		if err != nil {
			return fmt.Errorf("store: put service: %w", err)
		}
		return nil
	})
	if err != nil {
		return model.Service{}, err
	}
	svc.CreatedAt = fromNanos(created)
	return svc, nil
}

// checkListenAddr reports a conflict when a service other than id listens on
// the same socket as addr.
func checkListenAddr(ctx context.Context, tx *sql.Tx, id, addr string) error {
	key, checked := listenKey(addr)
	if !checked {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, listen_addr FROM services WHERE id <> ?`, id)
	if err != nil {
		return fmt.Errorf("store: put service: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, other string
		if err := rows.Scan(&name, &other); err != nil {
			return fmt.Errorf("store: put service: %w", err)
		}
		if k, ok := listenKey(other); ok && k == key {
			return conflict("listen address %q is already in use by service %q (%s)", addr, name, other)
		}
	}
	return rows.Err()
}

// sameServiceName compares service names ignoring case and surrounding white
// space, as profile names are.
func sameServiceName(a, b string) bool { return sameProfileName(a, b) }

// checkServiceName reports a conflict when a service other than id already
// uses name. The services.name column is UNIQUE, but that compares bytes; this
// is the comparison users expect ("API" and "api" are one name). Services are
// few, which lets Go do the Unicode-aware comparison.
func checkServiceName(ctx context.Context, tx *sql.Tx, id, name string) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM services WHERE id <> ?`, id)
	if err != nil {
		return fmt.Errorf("store: put service: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var other string
		if err := rows.Scan(&other); err != nil {
			return fmt.Errorf("store: put service: %w", err)
		}
		if sameServiceName(other, name) {
			return conflict("service name %q is already in use", name)
		}
	}
	return rows.Err()
}

// listenKey returns the canonical form of a listen address, so that spellings
// of one socket compare equal, and whether the address takes part in the
// uniqueness check at all. A wildcard host ("", 0.0.0.0, ::, in any spelling)
// becomes "", other IPs their canonical text form (IPv4-mapped IPv6 addresses
// become IPv4) and host names lower case; the port is read as a number. Port 0
// is never checked. A value that is not host:port is compared as a lower-case
// string.
func listenKey(addr string) (key string, checked bool) {
	addr = strings.TrimSpace(addr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.ToLower(addr), true
	}
	if n, err := strconv.ParseUint(port, 10, 16); err == nil {
		if n == 0 {
			return "", false
		}
		port = strconv.FormatUint(n, 10)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsUnspecified() {
			host = ""
		} else {
			host = ip.String()
		}
	} else {
		host = strings.ToLower(host)
	}
	return net.JoinHostPort(host, port), true
}

// DeleteService removes a service. Its recording is kept.
func (s *SQLite) DeleteService(ctx context.Context, id string) error {
	return s.deleteByID(ctx, "services", "service", id)
}
