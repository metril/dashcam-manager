package dashcam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
)

// Segment mirrors one entry of GET /api/ingest/manifest's segments array
// (see resources/ingest.py's IngestController.build_manifest).
type Segment struct {
	Camera        string            `json:"camera"`
	Path          string            `json:"path"`
	URL           string            `json:"url"`
	Size          int64             `json:"size"`
	MTime         string            `json:"mtime"`
	StartTime     string            `json:"start_time"`
	EndTime       string            `json:"end_time"`
	HasDetections bool              `json:"has_detections"`
	HasGPS        bool              `json:"has_gps"`
	HasTracks     bool              `json:"has_tracks"`
	Sidecars      map[string]string `json:"sidecars"`
}

// ManifestPage is one page of GET /api/ingest/manifest. NextSince decodes
// JSON null the same as an absent/empty field (""), but the package also
// remembers that it *was* null (nextSinceNull) so EachSegment can tell
// "no cursor to page with" apart from "cursor is the empty string" — the
// latter would otherwise make the next Manifest call restart from the very
// beginning instead of stopping.
type ManifestPage struct {
	Segments      []Segment `json:"segments"`
	Count         int       `json:"count"`
	NextSince     string    `json:"next_since"`
	nextSinceNull bool
}

// UnmarshalJSON distinguishes next_since: null (nextSinceNull=true) from
// next_since: "" or an absent key (both leave NextSince as "" without
// setting nextSinceNull) — see the type doc comment.
func (p *ManifestPage) UnmarshalJSON(data []byte) error {
	var aux struct {
		Segments  []Segment `json:"segments"`
		Count     int       `json:"count"`
		NextSince *string   `json:"next_since"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	p.Segments = aux.Segments
	p.Count = aux.Count
	if aux.NextSince != nil {
		p.NextSince = *aux.NextSince
		p.nextSinceNull = false
	} else {
		p.NextSince = ""
		p.nextSinceNull = true
	}
	return nil
}

// Manifest fetches one page of the segment manifest. limit <= 0 omits the
// limit query param (server default applies).
func (c *Client) Manifest(ctx context.Context, since, camera string, limit int) (ManifestPage, error) {
	v := url.Values{}
	if since != "" {
		v.Set("since", since)
	}
	if camera != "" {
		v.Set("camera", camera)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	pathQS := "/api/ingest/manifest" + buildQuery(v)

	resp, err := c.do(ctx, "GET", pathQS, nil)
	if err != nil {
		return ManifestPage{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return ManifestPage{}, fmt.Errorf("dashcam: read manifest: %w", err)
	}
	var page ManifestPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return ManifestPage{}, fmt.Errorf("dashcam: decode manifest: %w", err)
	}
	return page, nil
}

// EachSegment pages through the manifest via NextSince, calling fn for
// every segment in encounter order, and returns the cursor it stopped at.
// It stops when a page comes back with zero segments, when next_since
// comes back JSON null (no further cursor to page with), or — the
// sticky-cursor guard — when a page contains no path that wasn't already
// in the previous page (the server can legitimately repeat next_since on
// an unchanged page; without this guard that would loop forever
// re-fetching it). Subset, not exact-set, comparison: a page that's
// merely missing some paths the previous page had (e.g. a shrinking
// limit) is still "nothing new" and should stop too.
func (c *Client) EachSegment(ctx context.Context, since, camera string, limit int, fn func(Segment) error) (string, error) {
	return c.eachSegment(ctx, since, camera, limit, nil, fn)
}

// EachSegmentPaged is EachSegment but also invokes onPage(cursor), when
// non-nil, once a page has been fully delivered to fn and its next_since
// cursor is known — used by the dashcam_pull job to persist a resume
// cursor after every manifest page rather than only once the whole walk
// finishes, so a pull interrupted partway through a large backlog resumes
// from the last completed page on retry instead of restarting from since.
func (c *Client) EachSegmentPaged(ctx context.Context, since, camera string, limit int, onPage func(cursor string) error, fn func(Segment) error) (string, error) {
	return c.eachSegment(ctx, since, camera, limit, onPage, fn)
}

func (c *Client) eachSegment(ctx context.Context, since, camera string, limit int, onPage func(cursor string) error, fn func(Segment) error) (string, error) {
	cursor := since
	var prevPaths map[string]bool

	for {
		page, err := c.Manifest(ctx, cursor, camera, limit)
		if err != nil {
			return cursor, err
		}
		if len(page.Segments) == 0 {
			return cursor, nil
		}

		curPaths := make(map[string]bool, len(page.Segments))
		hasNew := prevPaths == nil
		for _, seg := range page.Segments {
			curPaths[seg.Path] = true
			if prevPaths != nil && !prevPaths[seg.Path] {
				hasNew = true
			}
		}
		if !hasNew {
			return cursor, nil
		}

		for _, seg := range page.Segments {
			if err := fn(seg); err != nil {
				return cursor, err
			}
		}

		if page.nextSinceNull {
			return cursor, nil
		}
		cursor = page.NextSince
		prevPaths = curPaths
		if onPage != nil {
			if err := onPage(cursor); err != nil {
				return cursor, err
			}
		}
	}
}

// SidecarRelPath extracts the rel path from a sidecar URL of the form
// "/api/ingest/file?path=<urlencoded rel path>" (the shape build_manifest
// emits for every sidecars entry and the top-level segment url).
func SidecarRelPath(sidecarURL string) (string, error) {
	u, err := url.Parse(sidecarURL)
	if err != nil {
		return "", fmt.Errorf("dashcam: parse sidecar url %q: %w", sidecarURL, err)
	}
	p := u.Query().Get("path")
	if p == "" {
		return "", fmt.Errorf("dashcam: sidecar url %q has no path param", sidecarURL)
	}
	return p, nil
}
