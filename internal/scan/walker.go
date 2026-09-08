package scan

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// foundSegment is one segment_NNNNN.mp4 file discovered on disk, with its
// path already decomposed and its os.FileInfo captured at walk time (so
// worker goroutines don't need to re-stat).
type foundSegment struct {
	RelPath string // slash-separated, relative to rootPath
	AbsPath string
	Size    int64
	MtimeUX int64
	SegPath SegmentPath
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
			found = append(found, foundSegment{
				RelPath: rel,
				AbsPath: p,
				Size:    info.Size(),
				MtimeUX: info.ModTime().Unix(),
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
