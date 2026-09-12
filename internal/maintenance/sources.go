package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"dashcam/internal/db"
	"dashcam/internal/share"
)

// isMountedFn is share.IsMounted, indirected so tests can stub it: real
// mount detection reads /proc/mounts, which a test's temp-dir source
// fixture never has an entry in (it's a plain directory, not something
// actually mounted there) — see maintenance_test.go's seedFixture, which
// stubs this directly instead of relying on a real mount.
var isMountedFn = share.IsMounted

// SetIsMountedFnForTest overrides checkSources' mount-detection hook,
// returning a restore func. isMountedFn itself is only stubbable from
// within this package (an unexported var) — this exists so tests in other
// packages (e.g. cmd/api's HTTP-level maintenance tests, which exercise
// checkSources indirectly through Verify/Prune) can still fake an
// otherwise-unmountable 'ssd'-kind source as available, the same way
// in-package tests do. Not for production use.
func SetIsMountedFnForTest(fn func(string) (bool, error)) (restore func()) {
	orig := isMountedFn
	isMountedFn = fn
	return func() { isMountedFn = orig }
}

// sourceStatus is one row of `sources` plus whether its root_path is
// currently trustworthy to walk.
type sourceStatus struct {
	ID        int64
	Label     string
	RootPath  string
	Available bool
	Reason    string // set when !Available
}

// checkSources reports every source's current availability: the directory
// named by root_path must exist, and — unless this is a "share"-kind
// source backed by a kind=local share (db.KindLocal; a local directory has
// no reason to have a /proc/mounts entry at all, so mount detection
// wouldn't mean anything for it — see share.LocalDirReadable) —
// share.IsMounted must confirm it's actually mounted there. This is a
// read-only availability check: a kind=local share uses
// share.LocalDirReadable, never share.ProbeLocal's write probe, since
// checkSources must not fail (or write anything) just because a local
// storage happens to be mounted read-only, and Verify/Prune's own
// consumers of this only ever read.
//
// This exists because a source's root_path only means anything while the
// removable SSD/share is actually mounted at it: an unplugged SSD leaves
// its root_path pointing at an empty (or missing) directory, which would
// otherwise make findMissingMediaSegments conclude every single segment's
// media file is gone — exactly the failure mode that made Verify/Prune
// unsafe to run against an unmounted source (a prune would then delete
// the entire library). Callers must refuse to classify segments under an
// unavailable source as missing, and Prune must refuse to run at all
// while any source is unavailable (see prune.go).
func checkSources(ctx context.Context, store *db.DB) ([]sourceStatus, error) {
	rows, err := store.Read().QueryContext(ctx, `SELECT id, label, root_path, kind, share_id FROM sources`)
	if err != nil {
		return nil, fmt.Errorf("maintenance: check sources: %w", err)
	}
	defer rows.Close()

	var out []sourceStatus
	for rows.Next() {
		var s sourceStatus
		var kind string
		var shareID sql.NullInt64
		if err := rows.Scan(&s.ID, &s.Label, &s.RootPath, &kind, &shareID); err != nil {
			return nil, fmt.Errorf("maintenance: check sources: %w", err)
		}

		if _, statErr := os.Stat(s.RootPath); statErr != nil {
			s.Reason = fmt.Sprintf("root path %s: %v", s.RootPath, statErr)
			out = append(out, s)
			continue
		}

		if kind == "share" && shareID.Valid {
			if sh, shErr := store.ShareGet(ctx, shareID.Int64); shErr == nil && sh.Kind == db.KindLocal {
				if lerr := share.LocalDirReadable(s.RootPath); lerr != nil {
					s.Reason = lerr.Error()
				} else {
					s.Available = true
				}
				out = append(out, s)
				continue
			}
		}

		mounted, mountErr := isMountedFn(s.RootPath)
		switch {
		case mountErr != nil:
			s.Reason = fmt.Sprintf("checking whether %s is mounted: %v", s.RootPath, mountErr)
		case !mounted:
			s.Reason = fmt.Sprintf("%s is not mounted", s.RootPath)
		default:
			s.Available = true
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// splitSources partitions checkSources' result into the set of available
// source ids (for scoping findMissingMediaSegments) and a human-readable
// reason string per unavailable source (for an error message / report
// field).
func splitSources(sources []sourceStatus) (available map[int64]bool, unavailable []string) {
	available = map[int64]bool{}
	for _, s := range sources {
		if s.Available {
			available[s.ID] = true
			continue
		}
		label := s.Label
		if label == "" {
			label = fmt.Sprintf("source %d", s.ID)
		}
		unavailable = append(unavailable, fmt.Sprintf("%s: %s", label, s.Reason))
	}
	return available, unavailable
}
