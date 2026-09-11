package dashcam

import (
	"context"
	"encoding/json"
	"fmt"
)

// ReprocessRequest is the body of POST /api/ingest/reprocess. Tracking is
// a pointer because the server has its own default (true, for this
// ingest-driven route — see resources/ingest_server.py's
// `body.setdefault('tracking', True)`); nil omits the field so that
// default applies, matching the Python client's own behavior of only
// sending fields it has a value for.
type ReprocessRequest struct {
	Camera      string
	Date        string
	Path        string
	Segment     string
	OnlyMissing bool
	SkipFrames  bool
	Tracking    *bool
}

// ReprocessStarted is POST /api/ingest/reprocess's success response.
type ReprocessStarted struct {
	Status string `json:"status"`
	Camera string `json:"camera"`
	Date   string `json:"date"`
	Engine string `json:"engine"`
}

// StartReprocess kicks off a reprocess run (requires an active maintenance
// lease with inference off — see resources/ingest_server.py's
// _handle_reprocess — or the server answers 409 maintenance_required).
func (c *Client) StartReprocess(ctx context.Context, req ReprocessRequest) (ReprocessStarted, error) {
	body := map[string]any{}
	if req.Camera != "" {
		body["camera"] = req.Camera
	}
	if req.Date != "" {
		body["date"] = req.Date
	}
	if req.Path != "" {
		body["path"] = req.Path
	}
	if req.Segment != "" {
		body["segment"] = req.Segment
	}
	if req.OnlyMissing {
		body["only_missing"] = true
	}
	if req.SkipFrames {
		body["skip_frames"] = true
	}
	if req.Tracking != nil {
		body["tracking"] = *req.Tracking
	}
	raw, _ := json.Marshal(body)

	resp, err := c.do(ctx, "POST", "/api/ingest/reprocess", raw)
	if err != nil {
		return ReprocessStarted{}, err
	}
	defer resp.Body.Close()
	var started ReprocessStarted
	if err := json.NewDecoder(resp.Body).Decode(&started); err != nil {
		return ReprocessStarted{}, fmt.Errorf("dashcam: decode reprocess start: %w", err)
	}
	return started, nil
}

// ReprocessStatus is GET /api/ingest/reprocess/status's shape. State is
// "idle" when nothing is running; any other value carries the rest of the
// fields.
type ReprocessStatus struct {
	State            string `json:"state"`
	Camera           string `json:"camera"`
	Date             string `json:"date"`
	Engine           string `json:"engine"`
	CurrentSegment   string `json:"current_segment"`
	Error            string `json:"error"`
	StartedAt        string `json:"started_at"`
	TotalSegments    int    `json:"total_segments"`
	Processed        int    `json:"processed"`
	TotalObjects     int    `json:"total_objects"`
	TotalPlates      int    `json:"total_plates"`
	TotalVehicles    int    `json:"total_vehicles"`
	TotalTrackEvents int    `json:"total_track_events"`
	Tracking         bool   `json:"tracking"`
}

// ReprocessStatus polls the running (or last-known) reprocess job's state.
func (c *Client) ReprocessStatus(ctx context.Context) (ReprocessStatus, error) {
	resp, err := c.do(ctx, "GET", "/api/ingest/reprocess/status", nil)
	if err != nil {
		return ReprocessStatus{}, err
	}
	defer resp.Body.Close()
	var st ReprocessStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return ReprocessStatus{}, fmt.Errorf("dashcam: decode reprocess status: %w", err)
	}
	return st, nil
}
