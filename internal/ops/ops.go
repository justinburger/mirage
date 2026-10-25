// Package ops implements recording-level operations shared by the API and
// the CLI: anonymized copies, HAR import/export, and loading engines and
// fault rules from the store.
//
// CONTRACT (implemented by the api agent):
//
//   - Everything streams: iterate with store.IterateExchanges and write in
//     batches of 500 with store.AppendExchanges; never hold a whole
//     recording in memory. Respect ctx cancellation between batches.
//   - CopyAnonymized creates the destination recording (Source "copy",
//     Anonymized true, Description noting the source and profile) before
//     copying; on error it deletes the partial destination.
//   - ImportHAR creates a recording (Source "har"), streams entries with
//     har.DecodeEntries (constant memory), converts them with
//     har.ToExchange (skipping entries whose error wraps har.ErrUnsupported,
//     e.g. data: URLs, and counting them in the recording description), fills
//     Endpoint with the normalizer from the raw escaped path BEFORE
//     anonymizing, anonymizes when eng is non-nil and appends in batches.
//   - ExportHAR streams with har.Encoder, anonymizing each exchange first
//     when eng is non-nil.
//   - LoadEngine reads the profile (profileID "" = model.DefaultProfileID),
//     applies WithDefaults and builds anonymize.New. A stored profile without
//     a secret is an error (tokens would not be stable).
//   - LoadFaults reads every fault rule and calls inj.SetRules.
package ops

import (
	"context"
	"io"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/pathnorm"
	"github.com/justinburger/mirage/internal/store"
)

// Progress reports exchanges processed so far (may be nil).
type Progress func(done int64)

// CopyAnonymized copies recording srcID into a new recording named name,
// anonymizing every exchange with eng.
func CopyAnonymized(ctx context.Context, st store.Store, srcID string, eng *anonymize.Engine, name string, progress Progress) (model.Recording, error) {
	panic("ops: not implemented")
}

// ImportHAR imports a HAR document into a new recording.
func ImportHAR(ctx context.Context, st store.Store, r io.Reader, name string, eng *anonymize.Engine, n *pathnorm.Normalizer) (model.Recording, error) {
	panic("ops: not implemented")
}

// ExportHAR writes a recording as a HAR document.
func ExportHAR(ctx context.Context, st store.Store, recordingID string, w io.Writer, eng *anonymize.Engine, version string) error {
	panic("ops: not implemented")
}

// LoadEngine builds the anonymization engine for a stored profile.
func LoadEngine(ctx context.Context, st store.Store, profileID string) (*anonymize.Engine, error) {
	panic("ops: not implemented")
}

// LoadFaults loads every stored fault rule into the injector.
func LoadFaults(ctx context.Context, st store.Store, inj *fault.Injector) error {
	panic("ops: not implemented")
}
