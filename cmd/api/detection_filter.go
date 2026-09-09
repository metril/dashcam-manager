// detection_filter.go defines DetectionFilter, the shape shared by three
// call sites: exportRequest.OverlayFilter (which detection boxes get
// burned into an export), the "detection_filter" settings key (the
// browser UI's persisted default filter), and — indirectly — the tracks
// array served by GET /api/segments/{id}/detections, which the frontend
// filters live against whichever DetectionFilter is in effect.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dashcam/internal/scan"
)

// DetectionFilter selects which detection boxes/tracks are "on": a
// minimum confidence, one bool per detection kind, and an optional class
// whitelist (empty = all object classes). JSON field names are snake_case
// to match the rest of the API (see cmd/api/handlers_search.go's
// min_conf, for one).
type DetectionFilter struct {
	MinConf  float64  `json:"min_conf"`
	Objects  bool     `json:"objects"`
	Plates   bool     `json:"plates"`
	Vehicles bool     `json:"vehicles"`
	Classes  []string `json:"classes"`
}

// DefaultDetectionFilter is the out-of-the-box filter: everything shown,
// classes unrestricted, and a 0.5 minimum confidence (the same floor
// Search.tsx's own min-confidence slider defaults its low end to) rather
// than 0 — an unfiltered burn-in/track list is dominated by low-confidence
// noise boxes otherwise.
func DefaultDetectionFilter() DetectionFilter {
	return DetectionFilter{
		MinConf:  0.5,
		Objects:  true,
		Plates:   true,
		Vehicles: true,
		Classes:  []string{},
	}
}

// UnmarshalJSON implements json.Unmarshaler with "absent (or null) field
// means default" semantics, rather than encoding/json's normal zero-value
// behavior. Fix round 1: a partial body like {"min_conf": 0.7} — someone
// only tightening the confidence floor — previously decoded straight into
// a DetectionFilter{} zero value, silently turning Objects/Plates/Vehicles
// all false (so an export with that overlay_filter burned in nothing, and
// a PUT /api/settings with it persisted an all-off filter). Every omitted
// field, and the whole value being JSON null, now falls back to
// DefaultDetectionFilter() instead.
func (f *DetectionFilter) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*f = DefaultDetectionFilter()
		return nil
	}
	var partial struct {
		MinConf  *float64  `json:"min_conf"`
		Objects  *bool     `json:"objects"`
		Plates   *bool     `json:"plates"`
		Vehicles *bool     `json:"vehicles"`
		Classes  *[]string `json:"classes"`
	}
	if err := json.Unmarshal(data, &partial); err != nil {
		return err
	}
	*f = DefaultDetectionFilter()
	if partial.MinConf != nil {
		f.MinConf = *partial.MinConf
	}
	if partial.Objects != nil {
		f.Objects = *partial.Objects
	}
	if partial.Plates != nil {
		f.Plates = *partial.Plates
	}
	if partial.Vehicles != nil {
		f.Vehicles = *partial.Vehicles
	}
	if partial.Classes != nil {
		f.Classes = *partial.Classes
	}
	return nil
}

// validDetectionClass reports whether class is one of the object classes
// the detection model actually emits (internal/scan.DetectionClasses,
// documented alongside internal/scan.DetectionFrame and the det_tracks
// schema comment in internal/db/schema.go).
func validDetectionClass(class string) bool {
	for _, c := range scan.DetectionClasses {
		if c == class {
			return true
		}
	}
	return false
}

// Validate clamps MinConf into [0,1] in place and rejects any Classes
// entry outside scan.DetectionClasses — the same two checks apply
// whether the filter arrives via POST /api/export's overlay_filter or a
// PUT /api/settings detection_filter value, so both call this rather than
// duplicating the rules.
func (f *DetectionFilter) Validate() error {
	if f.MinConf < 0 {
		f.MinConf = 0
	}
	if f.MinConf > 1 {
		f.MinConf = 1
	}
	for _, c := range f.Classes {
		if !validDetectionClass(c) {
			return fmt.Errorf("unknown class %q (want one of %v)", c, scan.DetectionClasses)
		}
	}
	return nil
}

// allowsClass reports whether f's class whitelist admits class — an empty
// Classes list means "all classes", matching DefaultDetectionFilter.
func (f *DetectionFilter) allowsClass(class string) bool {
	if len(f.Classes) == 0 {
		return true
	}
	for _, c := range f.Classes {
		if c == class {
			return true
		}
	}
	return false
}
