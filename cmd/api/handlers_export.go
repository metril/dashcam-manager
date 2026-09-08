// handlers_export.go implements M11's clip export: POST /api/export
// (validate + enqueue an "export" job), GET /api/exports (list),
// GET /api/exports/{id}/download (stream the finished file), DELETE
// /api/exports/{id}, plus the "export" job handler itself (registered from
// jobs_runtime.go) that does the actual planning/ffmpeg work by calling
// into internal/media's stitch.go/overlay.go.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"dashcam/internal/db"
	"dashcam/internal/jobs"
	"dashcam/internal/media"
	"dashcam/internal/scan"
	"dashcam/internal/share"
)

// exportExpiryDays is how long a download-target export's cache file is
// kept before the cleanup loop removes it (share-target exports, which
// land on a share mount rather than $DASHCAM_CACHE, never expire this
// way).
const exportExpiryDays = 7

// ---- HTTP request/response shapes ----

// exportItemParams is one requested clip range, shared between the HTTP
// request body and the "export" job's params_json (the handler enqueues
// exactly this shape). InS/OutS are segment-local seconds; nil means
// "from the start" / "to the end" respectively.
type exportItemParams struct {
	SegmentID int64    `json:"segment_id"`
	InS       *float64 `json:"in_s,omitempty"`
	OutS      *float64 `json:"out_s,omitempty"`
}

// exportJobParams is the "export" job's params_json shape.
type exportJobParams struct {
	ExportID int64              `json:"export_id"`
	Items    []exportItemParams `json:"items"`
	Camera   string             `json:"camera"`  // "front" | "rear" | "both"
	BurnIn   bool               `json:"burn_in"`
	Target   string             `json:"target"`  // "" / "download", or a share id
	Name     string             `json:"name"`
	Quality  string             `json:"quality"` // "source" | "1080p"
}

// exportRequest is the POST /api/export body.
type exportRequest struct {
	Items   []exportItemParams `json:"items"`
	Camera  string             `json:"camera"`
	BurnIn  bool               `json:"burn_in"`
	Target  string             `json:"target"`
	Name    string             `json:"name"`
	Quality string             `json:"quality"`
}

func (r exportRequest) validate() error {
	if len(r.Items) == 0 {
		return fmt.Errorf("items must not be empty")
	}
	switch r.Camera {
	case "front", "rear", "both":
	default:
		return fmt.Errorf("camera must be one of front, rear, both")
	}
	switch r.Quality {
	case "", "source", "1080p":
	default:
		return fmt.Errorf("quality must be one of source, 1080p")
	}
	if r.Target != "" && r.Target != "download" {
		if _, err := strconv.ParseInt(r.Target, 10, 64); err != nil {
			return fmt.Errorf("target must be \"download\" or a share id")
		}
	}
	for i, it := range r.Items {
		if it.SegmentID <= 0 {
			return fmt.Errorf("items[%d]: invalid segment_id", i)
		}
		if it.InS != nil && it.OutS != nil && *it.OutS <= *it.InS {
			return fmt.Errorf("items[%d]: out_s must be after in_s", i)
		}
	}
	return nil
}

// exportResponse is POST /api/export's response: both the created job (so
// the frontend can show live progress via SSE/GET /api/jobs/{id}) and the
// export row (so it can link straight to GET /api/exports/{id}).
type exportResponse struct {
	Job    *jobs.Job `json:"job"`
	Export db.Export `json:"export"`
}

// handleExportPost serves POST /api/export.
func handleExportPost(store *db.DB, q *jobs.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req exportRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := req.validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		name := req.Name
		if name == "" {
			name = "export-" + time.Now().UTC().Format("20060102-150405")
		}

		exp, err := store.ExportCreate(r.Context(), name)
		if err != nil {
			writeInternalError(w, "create export row", err)
			return
		}

		params := exportJobParams{
			ExportID: exp.ID,
			Items:    req.Items,
			Camera:   req.Camera,
			BurnIn:   req.BurnIn,
			Target:   req.Target,
			Name:     name,
			Quality:  req.Quality,
		}
		job, err := q.Enqueue(r.Context(), "export", params, 0)
		if err != nil {
			_ = store.ExportDelete(r.Context(), exp.ID)
			writeInternalError(w, "enqueue export job", err)
			return
		}
		if err := store.ExportSetJobID(r.Context(), exp.ID, job.ID); err != nil {
			log.Printf("[export] link export %d to job %d: %v", exp.ID, job.ID, err)
		} else {
			exp.JobID = &job.ID
		}

		writeJSON(w, http.StatusAccepted, exportResponse{Job: job, Export: exp})
	}
}

// handleExportsGet serves GET /api/exports.
func handleExportsGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ExportsList(r.Context())
		if err != nil {
			writeInternalError(w, "exports list", err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// handleExportDownloadGet serves GET /api/exports/{id}/download: streams
// the finished output file (download-target exports only — a share-target
// export's file lives on the share mount, not something this endpoint
// re-serves) with a Content-Disposition attachment header.
func handleExportDownloadGet(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid export id")
			return
		}
		exp, err := store.ExportGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "export not found")
			return
		}
		if err != nil {
			writeInternalError(w, "export get", err)
			return
		}
		if exp.Path == nil || *exp.Path == "" {
			writeJSONError(w, http.StatusConflict, "export is not ready or has no downloadable file")
			return
		}

		f, err := os.Open(*exp.Path)
		if err != nil {
			if os.IsNotExist(err) {
				writeJSONError(w, http.StatusNotFound, "export file not found on disk")
				return
			}
			writeInternalError(w, "open export file", err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			writeInternalError(w, "stat export file", err)
			return
		}

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", sanitizeExportFilename(exp.Name)+".mp4"))
		http.ServeContent(w, r, filepath.Base(*exp.Path), info.ModTime(), f)
	}
}

// handleExportDelete serves DELETE /api/exports/{id}: removes the row and,
// best-effort, its cache file (a share-target export's file on the share
// mount is left in place — deleting the export record shouldn't silently
// delete a file the user may have already copied elsewhere from the
// share).
func handleExportDelete(store *db.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid export id")
			return
		}
		exp, err := store.ExportGet(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "export not found")
			return
		}
		if err != nil {
			writeInternalError(w, "export get", err)
			return
		}
		if exp.ShareID == nil && exp.Path != nil && *exp.Path != "" {
			if rmErr := os.Remove(*exp.Path); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("[export] delete %d: remove file %s: %v", id, *exp.Path, rmErr)
			}
		}
		if err := store.ExportDelete(r.Context(), id); err != nil {
			writeInternalError(w, "export delete", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// exportFilenamePattern is what sanitizeExportFilename keeps; everything
// else becomes '-'.
var exportFilenamePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeExportFilename turns a user-supplied export name into a safe
// filename component (no path separators, no leading dot-dot).
func sanitizeExportFilename(name string) string {
	s := exportFilenamePattern.ReplaceAllString(name, "-")
	s = trimDashes(s)
	if s == "" || s == "." || s == ".." {
		return "export"
	}
	return s
}

func trimDashes(s string) string {
	for len(s) > 0 && (s[0] == '-' || s[0] == '.') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == '-' || s[len(s)-1] == '.') {
		s = s[:len(s)-1]
	}
	return s
}

// ---- registration ----

// registerExportJobHandler wires the "export" job type into q. Called once
// from registerJobHandlers (cmd/api/jobs_runtime.go) alongside the other
// production handlers.
func registerExportJobHandler(q *jobs.Queue, store *db.DB, cacheDir, device string) {
	q.RegisterHandler("export", exportJobHandler(store, cacheDir, device))
}

// ---- export job handler ----

// resolvedItem is one export item after resolving its DB row into a
// filesystem path and a concrete [InS,OutS) trim window.
type resolvedItem struct {
	info      db.SegmentExportInfo
	path      string
	inS, outS float64
	durationS float64
	whole     bool
}

func exportJobHandler(store *db.DB, cacheDir, device string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (any, error) {
		var p exportJobParams
		if err := json.Unmarshal(job.Params, &p); err != nil {
			return nil, fmt.Errorf("export job: invalid params: %w", err)
		}
		if len(p.Items) == 0 {
			return nil, fmt.Errorf("export job: no items")
		}

		if err := acquireFFmpegSlot(ctx); err != nil {
			return nil, err
		}
		defer releaseFFmpegSlot()

		report(0.02, "resolving segments")
		fronts, tripID, err := resolveExportFrontItems(ctx, store, p.Items, p.Camera)
		if err != nil {
			return nil, err
		}

		outW, outH := 0, 0
		if fronts[0].info.Width != nil && fronts[0].info.Height != nil {
			outW, outH = media.ResolveOutputDims(p.Quality, *fronts[0].info.Width, *fronts[0].info.Height)
		} else if pr, perr := media.Probe(ctx, fronts[0].path); perr == nil {
			outW, outH = media.ResolveOutputDims(p.Quality, pr.Width, pr.Height)
		}

		report(0.05, "planning")
		plan := planExport(ctx, fronts, p.Camera, p.BurnIn)

		cacheExportDir := filepath.Join(cacheDir, "exports")
		tmpDir := filepath.Join(cacheExportDir, "tmp")
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			return nil, fmt.Errorf("export job: tmp dir: %w", err)
		}
		out := media.ExportOutputPath(cacheDir, p.ExportID)

		progress := func(_ int64, _ float64) { report(0.5, "encoding") }

		if plan.FastPath {
			if err := runFastPathExport(ctx, fronts, tmpDir, job.ID, out, progress); err != nil {
				return nil, err
			}
		} else {
			if err := runReencodeExport(ctx, store, fronts, tripID, p, outW, outH, getHWMode(), device, tmpDir, job.ID, out, progress); err != nil {
				return nil, err
			}
		}

		info, statErr := os.Stat(out)
		var size int64
		if statErr == nil {
			size = info.Size()
		}

		finalPath, shareIDPtr, expiresAtPtr, err := finalizeExportOutput(ctx, store, out, p)
		if err != nil {
			return nil, err
		}
		if err := store.ExportFinish(ctx, p.ExportID, finalPath, shareIDPtr, size, expiresAtPtr); err != nil {
			log.Printf("[jobs] export %d: finish export row: %v", job.ID, err)
		}

		report(0.99, "done")
		return map[string]any{"export_id": p.ExportID, "path": finalPath, "bytes": size}, nil
	}
}

// resolveExportFrontItems resolves every item to its DB row + file path +
// trim window, validating that every segment matches camera (or, for
// "both", is a front-camera segment) and that they all belong to the same
// trip.
func resolveExportFrontItems(ctx context.Context, store *db.DB, items []exportItemParams, camera string) ([]resolvedItem, int64, error) {
	out := make([]resolvedItem, len(items))
	var tripID int64
	for i, it := range items {
		ri, err := resolveExportItem(ctx, store, it)
		if err != nil {
			return nil, 0, err
		}
		wantCamera := camera
		if camera == "both" {
			wantCamera = "front"
		}
		if ri.info.Camera != wantCamera {
			return nil, 0, fmt.Errorf("export job: segment %d is camera %q, want %q", it.SegmentID, ri.info.Camera, wantCamera)
		}
		if ri.info.TripID == nil {
			return nil, 0, fmt.Errorf("export job: segment %d has no trip", it.SegmentID)
		}
		if i == 0 {
			tripID = *ri.info.TripID
		} else if *ri.info.TripID != tripID {
			return nil, 0, fmt.Errorf("export job: items span more than one trip")
		}
		out[i] = ri
	}
	return out, tripID, nil
}

func resolveExportItem(ctx context.Context, store *db.DB, it exportItemParams) (resolvedItem, error) {
	info, err := store.SegmentExportInfoGet(ctx, it.SegmentID)
	if errors.Is(err, db.ErrNotFound) {
		return resolvedItem{}, fmt.Errorf("export job: segment %d not found", it.SegmentID)
	}
	if err != nil {
		return resolvedItem{}, fmt.Errorf("export job: segment %d: %w", it.SegmentID, err)
	}

	path, err := resolveMediaPath(info.SourceRoot, info.RelPath)
	if err != nil {
		return resolvedItem{}, fmt.Errorf("export job: segment %d: %w", it.SegmentID, err)
	}

	durationS := 0.0
	if info.DurationS != nil {
		durationS = *info.DurationS
	}
	if durationS <= 0 {
		pr, perr := media.Probe(ctx, path)
		if perr != nil {
			return resolvedItem{}, fmt.Errorf("export job: probe segment %d: %w", it.SegmentID, perr)
		}
		durationS = pr.DurationS
	}

	inS := 0.0
	if it.InS != nil {
		inS = *it.InS
	}
	if inS < 0 {
		inS = 0
	}
	outS := durationS
	if it.OutS != nil {
		outS = *it.OutS
	}
	if outS > durationS {
		outS = durationS
	}
	const eps = 0.05
	whole := inS <= eps && outS >= durationS-eps

	return resolvedItem{info: info, path: path, inS: inS, outS: outS, durationS: durationS, whole: whole}, nil
}

// keyframeToleranceS is how close a requested trim point must land to an
// actual keyframe (per ffprobe) to be treated as "aligned" — close enough
// that the fast concat-copy path won't visibly clip into the previous
// GOP's frames.
const keyframeToleranceS = 0.08

// keyframeProbeWindowS bounds how much of the file ffprobe scans around
// each trim point when checking alignment.
const keyframeProbeWindowS = 3.0

// planExport builds media.PlanItems (probing keyframe alignment for any
// partial-trim item via ffprobe) and returns the resulting Plan.
func planExport(ctx context.Context, fronts []resolvedItem, camera string, burnIn bool) media.Plan {
	items := make([]media.PlanItem, len(fronts))
	for i, f := range fronts {
		aligned := true
		if !f.whole {
			var err error
			aligned, err = itemKeyframeAligned(ctx, f)
			if err != nil {
				log.Printf("[export] keyframe probe segment %d: %v (forcing re-encode)", f.info.ID, err)
				aligned = false
			}
		}
		items[i] = media.PlanItem{Whole: f.whole, KeyframeAligned: aligned}
	}
	return media.BuildPlan(items, camera, burnIn)
}

func itemKeyframeAligned(ctx context.Context, f resolvedItem) (bool, error) {
	const eps = 0.05
	if f.inS > eps {
		times, err := media.ProbeKeyframeTimes(ctx, f.path, f.inS, keyframeProbeWindowS)
		if err != nil {
			return false, err
		}
		if !media.NearestKeyframe(times, f.inS, keyframeToleranceS) {
			return false, nil
		}
	}
	if f.outS < f.durationS-eps {
		times, err := media.ProbeKeyframeTimes(ctx, f.path, f.outS, keyframeProbeWindowS)
		if err != nil {
			return false, err
		}
		if !media.NearestKeyframe(times, f.outS, keyframeToleranceS) {
			return false, nil
		}
	}
	return true, nil
}

func runFastPathExport(ctx context.Context, fronts []resolvedItem, tmpDir string, jobID int64, out string, progress media.ProgressFunc) error {
	listPath := filepath.Join(tmpDir, fmt.Sprintf("%d.concat.txt", jobID))
	defer os.Remove(listPath)

	entries := make([]media.ConcatEntry, len(fronts))
	for i, f := range fronts {
		e := media.ConcatEntry{Path: f.path}
		if !f.whole {
			e.InS, e.OutS = f.inS, f.outS
		}
		entries[i] = e
	}

	lf, err := os.Create(listPath)
	if err != nil {
		return fmt.Errorf("export job: create concat list: %w", err)
	}
	werr := media.WriteConcatList(lf, entries)
	cerr := lf.Close()
	if werr != nil {
		return fmt.Errorf("export job: write concat list: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("export job: close concat list: %w", cerr)
	}

	return media.RunConcatCopy(ctx, listPath, out, progress)
}

func runReencodeExport(ctx context.Context, store *db.DB, fronts []resolvedItem, tripID int64, p exportJobParams, outW, outH int, hwMode media.Mode, device, tmpDir string, jobID int64, out string, progress media.ProgressFunc) error {
	items := make([]media.ReencodeInput, len(fronts))
	for i, f := range fronts {
		var overlay string
		if p.BurnIn {
			frames, err := loadOverlayFrames(ctx, store, f.info.ID, f.inS, f.outS)
			if err != nil {
				log.Printf("[export] burn-in segment %d: %v", f.info.ID, err)
			} else if len(frames) > 0 {
				overlay = media.BuildDrawFilters(frames, outW, outH)
			}
		}
		items[i] = media.ReencodeInput{Path: f.path, InS: f.inS, OutS: f.outS, Overlay: overlay}
	}
	spec := media.ReencodeSpec{Items: items, OutW: outW, OutH: outH, Mode: hwMode, Device: device}

	if p.Camera == "both" {
		rearItems, err := resolveRearItems(ctx, store, fronts, tripID, p.BurnIn, outW, outH)
		if err != nil {
			return err
		}
		spec.Rear = rearItems
	}

	script, outLabel := media.BuildFilterComplex(spec)
	scriptPath := filepath.Join(tmpDir, fmt.Sprintf("%d.filter", jobID))
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		return fmt.Errorf("export job: write filter script: %w", err)
	}
	defer os.Remove(scriptPath)

	return media.RunReencode(ctx, spec, scriptPath, outLabel, out, progress)
}

// resolveRearItems resolves, for every front item, the paired rear
// segment (via the trip's pair_offset_s) and its corresponding trim
// window — see internal/media.ResolveRearSegment's doc comment for the
// offset math.
func resolveRearItems(ctx context.Context, store *db.DB, fronts []resolvedItem, tripID int64, burnIn bool, outW, outH int) ([]media.ReencodeInput, error) {
	pair, err := store.TripPairInfoGet(ctx, tripID)
	if err != nil {
		return nil, fmt.Errorf("export job: trip %d: %w", tripID, err)
	}
	if pair.PairOffsetS == nil {
		return nil, fmt.Errorf("export job: trip %d has no front/rear pairing offset", tripID)
	}

	frontPlacements, err := store.TripCameraPlacements(ctx, tripID, "front")
	if err != nil {
		return nil, err
	}
	frontOffsetByID := make(map[int64]float64, len(frontPlacements))
	for _, fp := range frontPlacements {
		frontOffsetByID[fp.SegmentID] = fp.OffsetS
	}

	rearPlacementsDB, err := store.TripCameraPlacements(ctx, tripID, "rear")
	if err != nil {
		return nil, err
	}
	if len(rearPlacementsDB) == 0 {
		return nil, fmt.Errorf("export job: trip %d has no rear footage", tripID)
	}
	rearByID := make(map[int64]db.TripSegmentPlacement, len(rearPlacementsDB))
	rearPlacements := make([]media.RearSegmentPlacement, len(rearPlacementsDB))
	for i, rp := range rearPlacementsDB {
		rearByID[rp.SegmentID] = rp
		rearPlacements[i] = media.RearSegmentPlacement{SegmentID: rp.SegmentID, OffsetS: rp.OffsetS, DurationS: rp.DurationS}
	}

	out := make([]media.ReencodeInput, len(fronts))
	for i, f := range fronts {
		frontOffsetS, ok := frontOffsetByID[f.info.ID]
		if !ok {
			return nil, fmt.Errorf("export job: segment %d: no trip placement", f.info.ID)
		}
		mid := frontOffsetS + (f.inS+f.outS)/2
		rearSegID, rearMidLocal, ok := media.ResolveRearSegment(rearPlacements, mid, *pair.PairOffsetS)
		if !ok {
			return nil, fmt.Errorf("export job: segment %d: no rear footage at that time", f.info.ID)
		}
		half := (f.outS - f.inS) / 2
		rearIn, rearOut := rearMidLocal-half, rearMidLocal+half
		if rearIn < 0 {
			rearIn = 0
		}
		rp := rearByID[rearSegID]
		if rearOut > rp.DurationS && rp.DurationS > 0 {
			rearOut = rp.DurationS
		}
		rpath, err := resolveMediaPath(rp.SourceRoot, rp.RelPath)
		if err != nil {
			return nil, fmt.Errorf("export job: rear segment %d: %w", rearSegID, err)
		}

		var overlay string
		if burnIn {
			frames, ferr := loadOverlayFrames(ctx, store, rearSegID, rearIn, rearOut)
			if ferr != nil {
				log.Printf("[export] burn-in rear segment %d: %v", rearSegID, ferr)
			} else if len(frames) > 0 {
				overlay = media.BuildDrawFilters(frames, outW, outH)
			}
		}
		out[i] = media.ReencodeInput{Path: rpath, InS: rearIn, OutS: rearOut, Overlay: overlay}
	}
	return out, nil
}

// loadOverlayFrames decodes segID's detections_raw blob and returns the
// burn-in frames falling within [inS,outS) of it, shifted so 0 is the
// trimmed clip's own start. Returns (nil, nil) if the segment simply has
// no detections (not an error — most segments won't).
func loadOverlayFrames(ctx context.Context, store *db.DB, segID int64, inS, outS float64) ([]media.Frame, error) {
	gz, err := store.SegmentDetectionsRaw(ctx, segID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := gunzip(gz)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	parsed, err := scan.ParseDetectionsBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse detections: %w", err)
	}

	var frames []media.Frame
	for _, df := range parsed.DetectionFrames {
		if df.PTSOffset < inS || df.PTSOffset >= outS {
			continue
		}
		var boxes []media.Box
		for _, o := range df.Objects {
			boxes = append(boxes, media.Box{X1: o.Norm.X1, Y1: o.Norm.Y1, X2: o.Norm.X2, Y2: o.Norm.Y2, Label: o.Class, Color: classBoxColor(o.Class)})
		}
		for _, pl := range df.Plates {
			boxes = append(boxes, media.Box{X1: pl.Norm.X1, Y1: pl.Norm.Y1, X2: pl.Norm.X2, Y2: pl.Norm.Y2, Label: pl.Text, Color: media.ColorCyan})
		}
		if len(boxes) == 0 {
			continue
		}
		frames = append(frames, media.Frame{AtS: df.PTSOffset - inS, Boxes: boxes})
	}
	return frames, nil
}

func classBoxColor(class string) media.BoxColor {
	if class == "person" {
		return media.ColorRed
	}
	return media.ColorAmber
}

// finalizeExportOutput moves the ffmpeg output at out to its final
// resting place: unchanged (download target — stays in $DASHCAM_CACHE,
// with a 7-day expiry) or onto a share's mount (share target, no expiry).
func finalizeExportOutput(ctx context.Context, store *db.DB, out string, p exportJobParams) (finalPath string, shareID *int64, expiresAt *string, err error) {
	if p.Target == "" || p.Target == "download" {
		expiry := time.Now().UTC().Add(exportExpiryDays * 24 * time.Hour).Format(time.RFC3339Nano)
		return out, nil, &expiry, nil
	}

	id, perr := strconv.ParseInt(p.Target, 10, 64)
	if perr != nil {
		return "", nil, nil, fmt.Errorf("export job: invalid share target %q: %w", p.Target, perr)
	}
	sh, gerr := store.ShareGet(ctx, id)
	if gerr != nil {
		return "", nil, nil, fmt.Errorf("export job: share %d: %w", id, gerr)
	}
	destDir := filepath.Join(share.MountPointFor(share.DefaultMountBase, sh.MountPoint), "exports")
	if mkErr := os.MkdirAll(destDir, 0o755); mkErr != nil {
		return "", nil, nil, fmt.Errorf("export job: share export dir: %w", mkErr)
	}
	destPath := filepath.Join(destDir, sanitizeExportFilename(p.Name)+".mp4")
	if mvErr := moveFile(out, destPath); mvErr != nil {
		return "", nil, nil, fmt.Errorf("export job: move to share: %w", mvErr)
	}
	return destPath, &id, nil, nil
}

// moveFile renames src to dst, falling back to copy+remove for a
// cross-device move (a share mount is a different filesystem than
// $DASHCAM_CACHE, so a plain os.Rename would fail with EXDEV).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

// ---- expiry cleanup ----

// exportCleanupInterval is how often RunExportCleanupLoop sweeps for
// expired exports.
const exportCleanupInterval = 1 * time.Hour

// RunExportCleanupLoop periodically removes expired download-target
// exports until ctx is done. Intended to run in its own goroutine for the
// life of the process (see main.go), mirroring internal/share.RunHealthLoop.
func RunExportCleanupLoop(ctx context.Context, store *db.DB) {
	ticker := time.NewTicker(exportCleanupInterval)
	defer ticker.Stop()
	if n, err := CleanupExpiredExports(ctx, store); err != nil {
		log.Printf("[export] cleanup: %v", err)
	} else if n > 0 {
		log.Printf("[export] cleanup: removed %d expired export(s)", n)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := CleanupExpiredExports(ctx, store); err != nil {
				log.Printf("[export] cleanup: %v", err)
			} else if n > 0 {
				log.Printf("[export] cleanup: removed %d expired export(s)", n)
			}
		}
	}
}

// CleanupExpiredExports removes every expired download-target export's
// cache file and DB row. Intended to be called periodically (see main.go).
func CleanupExpiredExports(ctx context.Context, store *db.DB) (int, error) {
	expired, err := store.ExportsExpired(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range expired {
		if e.Path != nil && *e.Path != "" {
			if rmErr := os.Remove(*e.Path); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("[export] cleanup: remove %s: %v", *e.Path, rmErr)
			}
		}
		if err := store.ExportDelete(ctx, e.ID); err != nil {
			log.Printf("[export] cleanup: delete row %d: %v", e.ID, err)
			continue
		}
		n++
	}
	return n, nil
}
