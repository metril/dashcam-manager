// queries_vocabulary.go implements DB.Search's q->structural rewrite cache
// (task 9 round 3, see queries_search.go's rewriteQAsStructural): the small
// set of exact terms ("car", "honda", "sedan", ...) that a free-text q can
// be turned into an index-driven structural filter for, instead of an FTS
// resolution.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// searchVocabularyTTL bounds how stale DB.searchVocabulary's cache can be —
// refreshed lazily on the next call once this elapses, or immediately after
// scan.IndexDetections writes new det_vehicles rows (see
// InvalidateSearchVocabulary), so a newly-seen make/type becomes
// rewrite-eligible promptly rather than up to 5 minutes later.
const searchVocabularyTTL = 5 * time.Minute

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

// searchVocabularyCache is DB.vocab — see searchVocabulary.
type searchVocabularyCache struct {
	mu      sync.Mutex
	terms   map[string]vocabTerm
	builtAt time.Time
}

// InvalidateSearchVocabulary forces the next Search call to rebuild its
// q-rewrite vocabulary (searchVocabulary) instead of waiting up to
// searchVocabularyTTL. Called by scan.IndexDetections after it commits new
// det_vehicles rows.
func (d *DB) InvalidateSearchVocabulary() {
	d.vocab.mu.Lock()
	d.vocab.builtAt = time.Time{}
	d.vocab.mu.Unlock()
}

// searchVocabulary returns the cached (rebuilt at most every
// searchVocabularyTTL, or immediately after InvalidateSearchVocabulary)
// term -> {class?, make?, type?} vocabulary used by DB.Search to rewrite an
// exact-match free-text query into a structural filter. Building it is two
// DISTINCT scans of the already-indexed det_vehicles.make/type columns
// (idx_det_vehicles_make / idx_det_vehicles_type) plus the fixed six
// object classes search already knows how to filter on structurally
// (searchClassColumns) — cheap even though, unlike every other query in
// this package, it isn't scoped to one search's own result page: it's a
// small, slowly-changing vocabulary (dozens of makes/types, not
// per-detection), not per-request work.
func (d *DB) searchVocabulary(ctx context.Context) (map[string]vocabTerm, error) {
	d.vocab.mu.Lock()
	defer d.vocab.mu.Unlock()

	if d.vocab.terms != nil && time.Since(d.vocab.builtAt) < searchVocabularyTTL {
		return d.vocab.terms, nil
	}

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

	d.vocab.terms = terms
	d.vocab.builtAt = time.Now()
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
