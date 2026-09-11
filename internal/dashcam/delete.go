package dashcam

import (
	"context"
	"encoding/json"
	"fmt"
)

// SkippedPath is one entry of DeleteResult.Skipped.
type SkippedPath struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DeleteResult is POST /api/ingest/delete's response.
type DeleteResult struct {
	Deleted []string      `json:"deleted"`
	Skipped []SkippedPath `json:"skipped"`
}

// Delete removes segments (and their sidecars) from the recorder. sha,
// when non-nil, is a per-path checksum guard — the server refuses to
// delete a path whose current sha256 doesn't match (reason
// "checksum_mismatch"), which is how a caller confirms a destination copy
// landed intact before letting the recorder free the SSD original.
func (c *Client) Delete(ctx context.Context, paths []string, sha map[string]string) (DeleteResult, error) {
	body := map[string]any{"paths": paths}
	if len(sha) > 0 {
		body["sha256"] = sha
	}
	raw, _ := json.Marshal(body)

	resp, err := c.do(ctx, "POST", "/api/ingest/delete", raw)
	if err != nil {
		return DeleteResult{}, err
	}
	defer resp.Body.Close()
	var result DeleteResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return DeleteResult{}, fmt.Errorf("dashcam: decode delete result: %w", err)
	}
	return result, nil
}
