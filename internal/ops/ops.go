// Package ops implements recording-level operations shared by the API and
// the CLI: anonymized copies, HAR import/export, and loading engines and
// fault rules from the store.
//
// # Behaviour
//
// Everything streams. Recordings are read with store.IterateExchanges and
// written with store.AppendExchanges in batches (500 exchanges or about 8 MiB
// of bodies, whichever comes first), so memory use does not depend on the size
// of the recording. Cancelling the context stops an operation between
// exchanges.
//
// Operations that create a recording (CopyAnonymized, ImportHAR) are
// all-or-nothing: when they fail after the destination was created, the
// partial destination is deleted again (on a context that survives the
// caller's cancellation) and the original error is returned.
//
//   - CopyAnonymized creates a recording with Source "copy" and Anonymized
//     true, whose description names the source recording and the profile.
//   - ImportHAR creates a recording with Source "har". Entries are decoded one
//     at a time with har.DecodeEntries and converted with har.ToExchange.
//     Entries that are not HTTP(S) exchanges (data: URLs and the like, errors
//     wrapping har.ErrUnsupported) are skipped and counted in the
//     recording's description; any other conversion error fails the import
//     with an error wrapping model.ErrValidation that names the entry. The
//     Endpoint of every exchange is computed from the raw escaped path before
//     anonymization. Malformed HAR input yields an error wrapping
//     model.ErrValidation; a failure of the reader itself (a dropped upload,
//     a size limit) is returned as is, wrapped with context.
//   - ExportHAR streams with har.Encoder through a buffered writer. It does
//     not finish the document when it fails midway, so a truncated export is
//     recognisably invalid JSON rather than a plausible short HAR.
//   - LoadEngine builds the engine of a stored profile; LoadFaults copies the
//     stored fault rules into an injector.
//
// Errors wrap the sentinels in package model (ErrNotFound, ErrValidation, ...)
// so the API can map them to HTTP status codes.
package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/justinburger/mirage/internal/anonymize"
	"github.com/justinburger/mirage/internal/fault"
	"github.com/justinburger/mirage/internal/model"
	"github.com/justinburger/mirage/internal/store"
)

// Progress reports exchanges processed so far (may be nil).
type Progress func(done int64)

// LoadEngine builds the anonymization engine for a stored profile.
// An empty profileID selects model.DefaultProfileID. A stored profile without
// a secret is an error, because its tokens would not be stable.
func LoadEngine(ctx context.Context, st store.Store, profileID string) (*anonymize.Engine, error) {
	if profileID == "" {
		profileID = model.DefaultProfileID
	}
	p, err := st.GetProfile(ctx, profileID)
	if err != nil {
		// The store's error already names the profile; naming it again here
		// would read 'load profile "x": profile "x": not found'.
		return nil, fmt.Errorf("ops: load profile: %w", err)
	}
	if p.Secret == "" {
		return nil, fmt.Errorf("ops: profile %q has no secret, so anonymized tokens would not be stable; regenerate its secret", profileID)
	}
	eng, err := anonymize.New(p.WithDefaults())
	if err != nil {
		return nil, fmt.Errorf("ops: profile %q: %w", profileID, err)
	}
	return eng, nil
}

// LoadFaults loads every stored fault rule into the injector, replacing its
// rule set. When the stored rules are invalid the injector keeps its previous
// rules and the returned error wraps a *model.ValidationError.
func LoadFaults(ctx context.Context, st store.Store, inj *fault.Injector) error {
	if inj == nil {
		return errors.New("ops: load fault rules: nil injector")
	}
	rules, err := st.ListFaultRules(ctx)
	if err != nil {
		return fmt.Errorf("ops: load fault rules: %w", err)
	}
	if err := inj.SetRules(rules); err != nil {
		return fmt.Errorf("ops: load fault rules: %w", err)
	}
	return nil
}
