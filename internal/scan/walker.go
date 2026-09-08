package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// foundSegment is one segment_NNNNN.mp4 file discovered on disk, with its
// path already decomposed and its stat info captured at walk time (so
// worker goroutines don't need to re-stat).
type foundSegment struct {
	RelPath string // slash-separated, relative to rootPath
	AbsPath string
	Size    int64
	// MtimeUX is max(mp4 mtime, every present sidecar's mtime) as a unix
	// timestamp — not just the mp4's own mtime. A sidecar (e.g.
	// .detections.json) can be written or rewritten after the mp4 file
	// itself (the recorder writes the video first, sidecars once analysis
	// finishes), so comparing only the mp4's mtime would make Scan's
	// unchanged-skip check miss a sidecar that appeared or changed after
	// the segment was already indexed.
	MtimeUX int64
	SegPath SegmentPath
}

// sidecarExts are the sidecar files (relative to a segment's base path,
// i.e. its .mp4 path with the extension stripped) whose mtimes contribute
// to foundSegment.MtimeUX.
var sidecarExts = []string{".timestamp.json", ".detections.json", ".gps.json"}

// combinedMtime returns max(mp4Mtime, mtime of every sidecarExts file that
// exists at base+ext).
func combinedMtime(base string, mp4Mtime int64) int64 {
	max := mp4Mtime
	for _, ext := range sidecarExts {
		if info, err := os.Stat(base + ext); err == nil {
			if m := info.ModTime().Unix(); m > max {
				max = m
			}
		}
	}
	return max
}

// walkRoot walks rootPath's front/ and rear/ subtrees (either or both may be
// absent — a rear-only fixture tree is valid), skipping junk entries
// (IsJunk) and anything that doesn't match the recorder's expected layout,
// and returns every segment_NNNNN.mp4 file found, sorted by RelPath for
// deterministic, reproducible processing order.
func walkRoot(rootPath string) ([]foundSegment, error) {
	var found []foundSegment
	for _, cam := range []string{"front", "rear"} {
		camRoot := filepath.Join(rootPath, cam)
		if info, err := os.Stat(camRoot); err != nil || !info.IsDir() {
			continue
		}
		err := filepath.WalkDir(camRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if p != camRoot && IsJunk(name) {
					return filepath.SkipDir
				}
				return nil
			}
			if IsJunk(name) || !strings.HasSuffix(name, ".mp4") {
				return nil
			}
			rel, err := filepath.Rel(rootPath, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			sp, err := ParseSegmentPath(rel)
			if err != nil {
				// Doesn't match the recorder's expected layout (e.g. a
				// stray file) — skip rather than fail the whole scan.
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			base := strings.TrimSuffix(p, ".mp4")
			found = append(found, foundSegment{
				RelPath: rel,
				AbsPath: p,
				Size:    info.Size(),
				MtimeUX: combinedMtime(base, info.ModTime().Unix()),
				SegPath: sp,
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].RelPath < found[j].RelPath })
	return found, nil
}
