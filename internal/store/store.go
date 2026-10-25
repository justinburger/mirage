// Package store persists Mirage state in a single SQLite database (pure Go,
// modernc.org/sqlite, no cgo).
//
// CONTRACT (implemented by the store agent):
//
//   - Open creates the database and schema if needed, enables WAL mode,
//     sets busy_timeout, and seeds the built-in "default" anonymization
//     profile (model.DefaultAnonymizationProfile with a model.NewSecret())
//     when no profile with that ID exists.
//   - Every method is safe for concurrent use.
//   - Not-found lookups return an error wrapping model.ErrNotFound; unique
//     violations (duplicate service name or listen_addr) wrap
//     model.ErrConflict.
//   - Put* methods upsert by ID. They assign an ID (model.NewID with prefix
//     "svc", "prof", "flt", "rpl") when ID is empty, set CreatedAt on insert
//     and UpdatedAt on every write, and return the stored object.
//   - Structured sub-objects (MatchConfig, rules, labels, replay config,
//     summary, series) may be stored as JSON columns.
//   - AppendExchanges is the hot write path: one transaction per batch, with
//     prepared statements. It assigns ID and Seq (continuing the recording's
//     sequence) to each exchange in place and updates the recording's
//     counters (ExchangeCount, ErrorCount, Bytes, FirstAt, LastAt,
//     UpdatedAt). Target: ≥ 20k exchanges/s with small bodies on a laptop.
//     Bodies may be compressed (e.g. gzip above a size threshold) as long as
//     reads return the original bytes.
//   - IterateExchanges streams in Seq order without loading the recording
//     into memory, loading bodies only when asked (IterOptions). Returning
//     an error from fn stops iteration and is returned.
//   - DeleteRecording deletes its exchanges too.
//   - EndpointStats groups a recording's exchanges by Endpoint and computes
//     count, errors (model.Exchange.IsError semantics), avg/p50/p95/p99/max
//     duration and status counts. Sorted by count descending.
//   - ListReplayJobs omits Series (it can be large); GetReplayJob includes it.
//   - Schema changes go through a simple versioned migration table so later
//     releases can migrate existing databases.
package store

import (
	"context"

	"github.com/justinburger/mirage/internal/model"
)

// Store is the persistence interface used by every other package.
type Store interface {
	// Services
	ListServices(ctx context.Context) ([]model.Service, error)
	GetService(ctx context.Context, id string) (model.Service, error)
	PutService(ctx context.Context, s model.Service) (model.Service, error)
	DeleteService(ctx context.Context, id string) error

	// Recordings
	CreateRecording(ctx context.Context, r model.Recording) (model.Recording, error) // assigns ID ("rec") if empty
	ListRecordings(ctx context.Context) ([]model.Recording, error)                   // newest first
	GetRecording(ctx context.Context, id string) (model.Recording, error)
	// UpdateRecording updates Name, Description, Labels, Anonymized and
	// Source only; counters are owned by AppendExchanges.
	UpdateRecording(ctx context.Context, r model.Recording) (model.Recording, error)
	DeleteRecording(ctx context.Context, id string) error

	// Exchanges
	AppendExchanges(ctx context.Context, recordingID string, exs []*model.Exchange) error
	ListExchanges(ctx context.Context, recordingID string, q model.ExchangeQuery) (items []model.ExchangeSummary, total int64, err error)
	GetExchange(ctx context.Context, id int64) (*model.Exchange, error)
	IterateExchanges(ctx context.Context, recordingID string, opts model.IterOptions, fn func(*model.Exchange) error) error
	EndpointStats(ctx context.Context, recordingID string) ([]model.EndpointStat, error)

	// Anonymization profiles (Secret is stored and returned; the API hides it)
	ListProfiles(ctx context.Context) ([]model.AnonymizationProfile, error)
	GetProfile(ctx context.Context, id string) (model.AnonymizationProfile, error)
	PutProfile(ctx context.Context, p model.AnonymizationProfile) (model.AnonymizationProfile, error)
	DeleteProfile(ctx context.Context, id string) error // deleting "default" returns an error wrapping model.ErrConflict

	// Fault rules (List sorts by Order, then Name)
	ListFaultRules(ctx context.Context) ([]model.FaultRule, error)
	GetFaultRule(ctx context.Context, id string) (model.FaultRule, error)
	PutFaultRule(ctx context.Context, r model.FaultRule) (model.FaultRule, error)
	DeleteFaultRule(ctx context.Context, id string) error

	// Replay jobs (List: newest first, without Series)
	ListReplayJobs(ctx context.Context) ([]model.ReplayJob, error)
	GetReplayJob(ctx context.Context, id string) (model.ReplayJob, error)
	PutReplayJob(ctx context.Context, j model.ReplayJob) (model.ReplayJob, error)
	DeleteReplayJob(ctx context.Context, id string) error

	// Settings is a small key/value table for miscellaneous state.
	GetSetting(ctx context.Context, key string) (string, error) // model.ErrNotFound if unset
	PutSetting(ctx context.Context, key, value string) error

	Close() error
}

// SQLite is the SQLite implementation of Store.
type SQLite struct {
	// implementation fields
}

var _ Store = (*SQLite)(nil)

// Open opens (creating if needed) the database at path. The special path
// ":memory:" opens a private in-memory database, used by tests across the
// codebase.
func Open(path string) (*SQLite, error) { panic("store: not implemented") }

func (s *SQLite) ListServices(ctx context.Context) ([]model.Service, error) {
	panic("store: not implemented")
}
func (s *SQLite) GetService(ctx context.Context, id string) (model.Service, error) {
	panic("store: not implemented")
}
func (s *SQLite) PutService(ctx context.Context, svc model.Service) (model.Service, error) {
	panic("store: not implemented")
}
func (s *SQLite) DeleteService(ctx context.Context, id string) error {
	panic("store: not implemented")
}
func (s *SQLite) CreateRecording(ctx context.Context, r model.Recording) (model.Recording, error) {
	panic("store: not implemented")
}
func (s *SQLite) ListRecordings(ctx context.Context) ([]model.Recording, error) {
	panic("store: not implemented")
}
func (s *SQLite) GetRecording(ctx context.Context, id string) (model.Recording, error) {
	panic("store: not implemented")
}
func (s *SQLite) UpdateRecording(ctx context.Context, r model.Recording) (model.Recording, error) {
	panic("store: not implemented")
}
func (s *SQLite) DeleteRecording(ctx context.Context, id string) error {
	panic("store: not implemented")
}
func (s *SQLite) AppendExchanges(ctx context.Context, recordingID string, exs []*model.Exchange) error {
	panic("store: not implemented")
}
func (s *SQLite) ListExchanges(ctx context.Context, recordingID string, q model.ExchangeQuery) ([]model.ExchangeSummary, int64, error) {
	panic("store: not implemented")
}
func (s *SQLite) GetExchange(ctx context.Context, id int64) (*model.Exchange, error) {
	panic("store: not implemented")
}
func (s *SQLite) IterateExchanges(ctx context.Context, recordingID string, opts model.IterOptions, fn func(*model.Exchange) error) error {
	panic("store: not implemented")
}
func (s *SQLite) EndpointStats(ctx context.Context, recordingID string) ([]model.EndpointStat, error) {
	panic("store: not implemented")
}
func (s *SQLite) ListProfiles(ctx context.Context) ([]model.AnonymizationProfile, error) {
	panic("store: not implemented")
}
func (s *SQLite) GetProfile(ctx context.Context, id string) (model.AnonymizationProfile, error) {
	panic("store: not implemented")
}
func (s *SQLite) PutProfile(ctx context.Context, p model.AnonymizationProfile) (model.AnonymizationProfile, error) {
	panic("store: not implemented")
}
func (s *SQLite) DeleteProfile(ctx context.Context, id string) error {
	panic("store: not implemented")
}
func (s *SQLite) ListFaultRules(ctx context.Context) ([]model.FaultRule, error) {
	panic("store: not implemented")
}
func (s *SQLite) GetFaultRule(ctx context.Context, id string) (model.FaultRule, error) {
	panic("store: not implemented")
}
func (s *SQLite) PutFaultRule(ctx context.Context, r model.FaultRule) (model.FaultRule, error) {
	panic("store: not implemented")
}
func (s *SQLite) DeleteFaultRule(ctx context.Context, id string) error {
	panic("store: not implemented")
}
func (s *SQLite) ListReplayJobs(ctx context.Context) ([]model.ReplayJob, error) {
	panic("store: not implemented")
}
func (s *SQLite) GetReplayJob(ctx context.Context, id string) (model.ReplayJob, error) {
	panic("store: not implemented")
}
func (s *SQLite) PutReplayJob(ctx context.Context, j model.ReplayJob) (model.ReplayJob, error) {
	panic("store: not implemented")
}
func (s *SQLite) DeleteReplayJob(ctx context.Context, id string) error {
	panic("store: not implemented")
}
func (s *SQLite) GetSetting(ctx context.Context, key string) (string, error) {
	panic("store: not implemented")
}
func (s *SQLite) PutSetting(ctx context.Context, key, value string) error {
	panic("store: not implemented")
}
func (s *SQLite) Close() error { panic("store: not implemented") }
