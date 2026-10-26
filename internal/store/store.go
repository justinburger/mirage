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
//   - Not-found lookups return an error wrapping model.ErrNotFound; uniqueness
//     violations (duplicate service name or listen address, duplicate profile
//     name) wrap model.ErrConflict.
//   - Put* methods upsert by ID. They assign an ID (model.NewID with prefix
//     "svc", "prof", "flt", "rpl") when ID is empty, set CreatedAt on insert
//     and UpdatedAt on every write, and return the stored object.
//   - PutProfile also assigns model.NewID("rule") to every rule with an empty
//     ID (existing IDs are kept) and requires the profile name to be unique,
//     ignoring case and surrounding white space.
//   - PutService requires the listen address to be unique among services,
//     compared host-normalised: ":9001", "0.0.0.0:9001" and "[::]:9001" are one
//     socket, while a specific host and a wildcard on one port, or "localhost"
//     and 127.0.0.1, are left to the OS to refuse at listen time. Port 0 (an
//     OS-assigned port) is exempt, so any number of services may use ":0" or
//     "127.0.0.1:0". The service name is unique as well.
//   - Structured sub-objects (MatchConfig, rules, labels, replay config,
//     summary, series) may be stored as JSON columns.
//   - AppendExchanges is the hot write path: one transaction per batch, with
//     prepared statements. It assigns ID and Seq (continuing the recording's
//     sequence) to each exchange in place and updates the recording's
//     counters (ExchangeCount, ErrorCount, Bytes, FirstAt, LastAt,
//     UpdatedAt) and its Anonymized flag. Target: ≥ 20k exchanges/s with small
//     bodies on a laptop.
//     Bodies may be compressed (e.g. gzip above a size threshold) as long as
//     reads return the original bytes.
//   - Recording.Source and the initial Anonymized value are set by
//     CreateRecording. AppendExchanges then maintains Anonymized: appending any
//     exchange with Anonymized=false clears it, and appending to an empty
//     recording sets it to whether every appended exchange is anonymized (so a
//     recording created non-anonymized but filled only with anonymized captures
//     becomes true). On a non-empty recording the flag is never set, only
//     cleared. UpdateRecording writes neither Source nor Anonymized: it edits
//     Name, Description and Labels only.
//   - IterateExchanges streams in Seq order without loading the recording
//     into memory, loading bodies only when asked (IterOptions). Returning
//     an error from fn stops iteration and is returned.
//   - DeleteRecording deletes its exchanges too, without monopolising the single
//     writer: the recording disappears first (every read and write reports it
//     as not found, and its ID stays reserved), then its exchanges are deleted
//     in chunks of at most 10,000 rows per transaction, so capture batches of
//     other recordings interleave, and finally the recording row. The call is
//     synchronous; see the implementation notes for interruptions.
//   - EndpointStats groups a recording's exchanges by Endpoint and computes
//     count, errors (model.Exchange.IsError semantics), avg/p50/p95/p99/max
//     duration and status counts. Sorted by count descending.
//   - ListReplayJobs omits Series (it can be large); GetReplayJob includes it.
//   - Schema changes go through a simple versioned migration table so later
//     releases can migrate existing databases.
//
// # Implementation notes
//
// Connections. A file database is opened through two database/sql pools on
// the same file: a writer pool limited to one connection (SQLite has a single
// writer, so writers queue in Go instead of spinning on SQLITE_BUSY and every
// write transaction starts with BEGIN IMMEDIATE) and a reader pool of several
// query_only connections. In WAL mode readers never block the writer and the
// writer never blocks readers, so a long IterateExchanges does not stall
// capture. The special path ":memory:" gives every Open its own private
// shared-cache in-memory database, served through one active connection (a
// second, pinned connection only keeps the database alive), so operations are
// serialised and always see committed data.
//
// Configuration objects (services, profiles, fault rules, replay jobs) keep
// their complete JSON encoding in a data column, next to a few denormalised
// scalar columns that carry uniqueness, ordering and lookups. The JSON is the
// source of truth, so a field added to the model is persisted without a schema
// change. Recordings and exchanges use real columns because SQL arithmetic and
// filters run on them.
//
// Exchanges. Every field ListExchanges filters or displays is a scalar column
// (indexed on recording_id with seq, endpoint, status and, partially, errors);
// headers are JSON text and bodies are BLOB columns stored last in the row and
// never selected by list queries. A body larger than 1 KiB is gzipped when
// that saves space (flagged per row) and is transparently inflated on read.
// Exchange IDs come from an AUTOINCREMENT key, so they are unique across
// recordings and never reused.
//
// Schema versions. 1 is the initial schema. 2 rebuilds the services table
// without the UNIQUE constraint on listen_addr, whose uniqueness PutService
// now checks itself (host-normalised, port 0 exempt; the name stays UNIQUE).
// 3 adds recordings.deleted_at, the tombstone of DeleteRecording. Profile name
// uniqueness is likewise checked by PutProfile, not by an index, so databases
// that already hold two profiles of one name still open; such a profile must
// be renamed before it can be saved again.
//
// Deleting a recording. DeleteRecording first sets deleted_at on the recording
// row in one short transaction. Every query that looks a recording up filters
// on deleted_at IS NULL, so from then on the recording is invisible and cannot
// be appended to, and CreateRecording refuses its ID (the row still exists).
// The exchanges are deleted by a loop of single DELETE statements, each limited
// to 10,000 rows and each its own transaction, so the writer connection is free
// between them and database/sql hands it to the longest-waiting writer; the
// recording row goes last (the foreign key cascade then has nothing to do). If
// the caller's context ends in between, the rest of the purge continues on a
// background goroutine and DeleteRecording returns the context's error. Close
// cancels and waits for such goroutines. A purge that never finished (Close, a
// crash) leaves a tombstoned recording, which Open finds and purges in the
// background; nothing else ever sees it.
//
// Timestamps are stored as Unix nanoseconds (the zero time.Time is stored as
// 0), which limits the representable range to the years 1678-2262, and are
// returned in UTC.
//
// Ordering of lists: services by name, profiles with "default" first and then
// by name, fault rules by Order then Name, recordings and replay jobs newest
// first.
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
	// PutService returns an error wrapping model.ErrConflict for a duplicate
	// name or a listen address another service uses (port 0 is exempt).
	PutService(ctx context.Context, s model.Service) (model.Service, error)
	DeleteService(ctx context.Context, id string) error

	// Recordings
	CreateRecording(ctx context.Context, r model.Recording) (model.Recording, error) // assigns ID ("rec") if empty
	ListRecordings(ctx context.Context) ([]model.Recording, error)                   // newest first
	GetRecording(ctx context.Context, id string) (model.Recording, error)
	// UpdateRecording updates Name, Description and Labels only. Source and
	// the initial Anonymized value are set by CreateRecording; AppendExchanges
	// owns the counters, the time range and Anonymized afterwards.
	UpdateRecording(ctx context.Context, r model.Recording) (model.Recording, error)
	// DeleteRecording removes the recording at once from every read and write,
	// then deletes its exchanges in chunks of at most 10,000 rows per
	// transaction (see the package comment).
	DeleteRecording(ctx context.Context, id string) error

	// Exchanges
	AppendExchanges(ctx context.Context, recordingID string, exs []*model.Exchange) error
	ListExchanges(ctx context.Context, recordingID string, q model.ExchangeQuery) (items []model.ExchangeSummary, total int64, err error)
	GetExchange(ctx context.Context, id int64) (*model.Exchange, error)
	IterateExchanges(ctx context.Context, recordingID string, opts model.IterOptions, fn func(*model.Exchange) error) error
	EndpointStats(ctx context.Context, recordingID string) ([]model.EndpointStat, error)

	// Anonymization profiles (Secret is stored and returned; the API hides it).
	// PutProfile assigns IDs to rules without one and returns an error wrapping
	// model.ErrConflict for a name another profile already uses (ignoring case).
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

var _ Store = (*SQLite)(nil)
