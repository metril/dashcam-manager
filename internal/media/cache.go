package media

import (
	"fmt"
	"path/filepath"
)

// shardDir buckets a segment id into segment_id % 1000 subdirectories, so
// a library with tens of thousands of segments doesn't dump every
// thumbnail/sprite into one flat directory.
func shardDir(segmentID int64) string {
	return fmt.Sprintf("%d", segmentID%1000)
}

// ThumbPath returns the cache-relative path for segment segmentID's
// thumbnail JPEG, under cacheDir (DASHCAM_CACHE).
func ThumbPath(cacheDir string, segmentID int64) string {
	return filepath.Join(cacheDir, "thumbs", shardDir(segmentID), fmt.Sprintf("%d.jpg", segmentID))
}

// SpritePaths returns the cache-relative jpg and vtt paths for segment
// segmentID's hover-scrub sprite sheet, under cacheDir.
func SpritePaths(cacheDir string, segmentID int64) (jpg, vtt string) {
	dir := filepath.Join(cacheDir, "sprites", shardDir(segmentID))
	return filepath.Join(dir, fmt.Sprintf("%d.jpg", segmentID)), filepath.Join(dir, fmt.Sprintf("%d.vtt", segmentID))
}
