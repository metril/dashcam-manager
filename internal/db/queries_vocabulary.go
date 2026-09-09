// queries_vocabulary.go implements DB.Search's q->structural rewrite cache
// (task 9 round 3, see queries_search.go's rewriteQAsStructural): the small
// set of exact terms ("car", "honda", "sedan", ...) that a free-text q can
// be turned into an index-driven structural filter for, instead of an FTS
// resolution.
//
// Fix round 4 (IMPORTANT 3): the ~1.4s vocabulary build (two DISTINCT
// scans over det_vehicles) used to run synchronously, under d.vocab.mu,
// inside whichever Search call happened to find the cache empty or
// expired — serializing every concurrent search behind it, and since
// scan.IndexDetections invalidates the cache per segment, a running
// tracks_backfill/scan job (touching thousands of segments) meant nearly
// every search during that window paid the full rebuild. The cache is now
// stale-while-revalidate: a request never runs the rebuild itself, only
// ever reads the last-published snapshot (or, before any snapshot has
// ever been published, skips the rewrite entirely) while at most one
// background goroutine refreshes it, debounced so a burst of
// invalidations triggers at most one rebuild per searchVocabularyDebounce
// window rather than one per segment.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// searchVocabularyTTL bounds how stale a published vocabulary snapshot is
// served before a background refresh is triggered.
const searchVocabularyTTL = 5 * time.Minute

// searchVocabularyDebounce bounds how often a background refresh can be
// *started* — protects against scan.IndexDetections calling
// InvalidateSearchVocabulary once per segment during a scan/backfill touching
// thousands of segments in quick succession.
const searchVocabularyDebounce = 30 * time.Second

// vocabTerm records which structural filter(s) a lowercased free-text term
// exactly matches. makeCanon/typeCanon preserve the term's actual on-disk
// casing: Search's Make/Type filters are exact-match (matchPredicate's
// "v.make = ?"/"v.type = ?"), not case-insensitive, but q is matched
// case-insensitively against this vocabulary, so the rewrite needs the
// real casing to still find rows.
type vocabTerm struct {
	class     bool
	makeCanon string // "" if term isn't a known vehicle make
	typeCanon string // "" if term isn't a known vehicle type
}

// searchVocabularyCache is DB.vocab — see the package doc comment above.
// terms is nil until the first successful build ever publishes one; every
// field is guarded by mu, and terms itself, once published, is never
// mutated in place (a refresh publishes a whole new map), so callers may
// read it after releasing mu without racing a concurrent refresh.
type searchVocabularyCache struct {
	mu      sync.Mutex
	terms   map[string]vocabTerm
	builtAt time.Time
	// dirty is set by InvalidateSearchVocabulary and cleared by a
	// completed refresh — distinct from TTL staleness so a refresh
	// triggered by an explicit invalidation and one triggered by TTL
	// expiry share the same debounce/single-flight machinery below.
	dirty bool
	// refreshing is true while a background rebuild goroutine is in
	// flight — the single-flight guard: never more than one at a time.
	refreshing bool
	// lastRefreshStart is when the most recent refresh goroutine was
	// started (not when it finished) — the debounce clock.
	lastRefreshStart time.Time
}

// InvalidateSearchVocabulary marks the cache stale and, respecting
// searchVocabularyDebounce, kicks a background refresh. Never blocks and
// never rebuilds inline — safe to call once per segment from
// scan.IndexDetections during a large scan/backfill.
func (d *DB) InvalidateSearchVocabulary() {
	d.vocab.mu.Lock()
	d.vocab.dirty = true
	d.maybeStartVocabRefreshLocked()
	d.vocab.mu.Unlock()
}

// maybeStartVocabRefreshLocked starts a background rebuild (see
// refreshSearchVocabulary) unless one is already running or one was
// started more recently than searchVocabularyDebounce ago. Caller must
// hold d.vocab.mu; it is released and re-acquired by no one here — the
// spawned goroutine acquires it independently once the (unlocked) DB
// query completes.
func (d *DB) maybeStartVocabRefreshLocked() {
	if d.vocab.refreshing {
		return
	}
	if !d.vocab.lastRefreshStart.IsZero() && time.Since(d.vocab.lastRefreshStart) < searchVocabularyDebounce {
		return
	}
	d.vocab.refreshing = true
	d.vocab.lastRefreshStart = time.Now()
	go d.refreshSearchVocabulary()
}

// refreshSearchVocabulary rebuilds the vocabulary and publishes it —
// always run on its own goroutine (see maybeStartVocabRefreshLocked), never
// on a request's goroutine. Uses context.Background(): the goroutine
// outlives whatever request (if any) triggered it.
func (d *DB) refreshSearchVocabulary() {
	terms, err := d.buildSearchVocabulary(context.Background())

	d.vocab.mu.Lock()
	defer d.vocab.mu.Unlock()
	d.vocab.refreshing = false
	if err != nil {
		// Leave the existing snapshot (possibly nil, possibly stale) in
		// place; the next debounce window retries. There's no request to
		// return this error to from here.
		return
	}
	d.vocab.terms = terms
	d.vocab.builtAt = time.Now()
	d.vocab.dirty = false
}

// searchVocabulary returns the last-published term -> {class?, make?,
// type?} vocabulary snapshot DB.Search's rewriteQAsStructural rewrites an
// exact-match free-text q against. Never blocks on a rebuild:
//   - No snapshot has ever been published (process just started, or every
//     build attempt so far has failed): kicks a background build (subject
//     to the debounce/single-flight guards) and returns ok=false —
//     rewriteQAsStructural treats that as "nothing to rewrite yet", so q
//     falls through to the unchanged FTS path until the first build
//     lands.
//   - A snapshot exists but is stale (TTL elapsed, or
//     InvalidateSearchVocabulary was called): the stale snapshot is still
//     returned immediately (stale-while-revalidate) while a background
//     refresh is kicked off the same way.
func (d *DB) searchVocabulary(ctx context.Context) (map[string]vocabTerm, bool) {
	d.vocab.mu.Lock()
	defer d.vocab.mu.Unlock()

	if d.vocab.terms == nil {
		d.maybeStartVocabRefreshLocked()
		return nil, false
	}
	if d.vocab.dirty || time.Since(d.vocab.builtAt) >= searchVocabularyTTL {
		d.maybeStartVocabRefreshLocked()
	}
	return d.vocab.terms, true
}

// buildSearchVocabulary runs the actual two DISTINCT scans (of the
// already-indexed det_vehicles.make/type columns,
// idx_det_vehicles_make/idx_det_vehicles_type) plus the fixed six object
// classes search already knows how to filter on structurally
// (searchClassColumns) — the ~1.4s-on-the-real-database cost this file's
// caching exists to keep off the request path. Called only from
// refreshSearchVocabulary's background goroutine (and directly by tests
// that need a synchronous, deterministic snapshot).
func (d *DB) buildSearchVocabulary(ctx context.Context) (map[string]vocabTerm, error) {
	terms := make(map[string]vocabTerm, len(searchClassColumns))
	for c := range searchClassColumns {
		terms[c] = vocabTerm{class: true}
	}

	makeRows, err := d.read.QueryContext(ctx, `SELECT DISTINCT make FROM det_vehicles WHERE make IS NOT NULL AND make != ''`)
	if err != nil {
		return nil, fmt.Errorf("db: search vocabulary: makes: %w", err)
	}
	if err := scanVocabColumn(makeRows, terms, func(t *vocabTerm, canon string) { t.makeCanon = canon }); err != nil {
		return nil, err
	}

	typeRows, err := d.read.QueryContext(ctx, `SELECT DISTINCT type FROM det_vehicles WHERE type IS NOT NULL AND type != ''`)
	if err != nil {
		return nil, fmt.Errorf("db: search vocabulary: types: %w", err)
	}
	if err := scanVocabColumn(typeRows, terms, func(t *vocabTerm, canon string) { t.typeCanon = canon }); err != nil {
		return nil, err
	}

	return terms, nil
}

// scanVocabColumn scans a single-column DISTINCT result (make or type)
// into terms, keyed by the lowercased/trimmed value, applying set to
// record that column's canonical (as-stored) casing on the term. Always
// closes rows.
func scanVocabColumn(rows *sql.Rows, terms map[string]vocabTerm, set func(*vocabTerm, string)) error {
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("db: search vocabulary: %w", err)
		}
		key := strings.ToLower(strings.TrimSpace(v))
		if key == "" {
			continue
		}
		t := terms[key]
		set(&t, v)
		terms[key] = t
	}
	return rows.Err()
}
