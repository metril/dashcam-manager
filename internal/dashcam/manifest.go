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

// ManifestPage is one page of GET /api/ingest/manifest.
type ManifestPage struct {
	Segments  []Segment `json:"segments"`
	Count     int       `json:"count"`
	NextSince string    `json:"next_since"`
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
// It stops when a page comes back with zero segments, or — the
// sticky-cursor guard — when a page's segments are exactly the same set of
// paths as the previous page's (the server can legitimately repeat
// next_since on an empty-looking page; without this guard that would loop
// forever re-fetching the same page).
func (c *Client) EachSegment(ctx context.Context, since, camera string, limit int, fn func(Segment) error) (string, error) {
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
		samePage := prevPaths != nil && len(prevPaths) == len(page.Segments)
		for _, seg := range page.Segments {
			curPaths[seg.Path] = true
			if samePage && !prevPaths[seg.Path] {
				samePage = false
			}
		}
		if samePage {
			return cursor, nil
		}

		for _, seg := range page.Segments {
			if err := fn(seg); err != nil {
				return cursor, err
			}
		}

		cursor = page.NextSince
		prevPaths = curPaths
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
