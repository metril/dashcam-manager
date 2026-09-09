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
	"dashcam/internal/ingest"
	"dashcam/internal/jobs"
	"dashcam/internal/media"
	"dashcam/internal/scan"
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
	Camera   string             `json:"camera"` // "front" | "rear" | "both"
	BurnIn   bool               `json:"burn_in"`
	Target   string             `json:"target"` // "" / "download", or a share id
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

// maxExportItems bounds POST /api/export's items list. An export runs
// synchronously through a single ffmpeg concat/re-encode pass (see
// planExport/reencodeExport) and every item means another probe + another
// keyframe-alignment/overlay pass — an unbounded list is both a
// pathological-request DoS vector and, in practice, not something anyone
// building a clip actually wants (200 segments is already ~100 minutes of
// dashcam footage at the recorder's ~30s segment length).
const maxExportItems = 200

func (r exportRequest) validate() error {
	if len(r.Items) == 0 {
		return fmt.Errorf("items must not be empty")
	}
	if len(r.Items) > maxExportItems {
		return fmt.Errorf("items must not exceed %d (got %d)", maxExportItems, len(r.Items))
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
		for _, it := range req.Items {
			seg, err := store.SegmentGet(r.Context(), it.SegmentID)
			if errors.Is(err, db.ErrNotFound) {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("segment %d not found", it.SegmentID))
				return
			}
			if err != nil {
				writeInternalError(w, "segment get", err)
				return
			}
			if seg.UnreadableReason != nil && *seg.UnreadableReason != "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("segment %d is unreadable (%s) and cannot be exported", it.SegmentID, *seg.UnreadableReason))
				return
			}
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
		if exp.ShareID != nil {
			writeJSONError(w, http.StatusConflict, "export was sent to a share, not downloadable — see its path on GET /api/exports")
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

// defaultOutputWidth/defaultOutputHeight are used when a segment's
// dimensions are unknown (unset in the DB and unprobable) — the recorder's
// documented resolution (see docs/superpowers/specs), so burn-in box
// coordinates and the re-encode's scale filter still land on something
// sane rather than silently drawing at 0x0.
const (
	defaultOutputWidth  = 3840
	defaultOutputHeight = 2160
)

func exportJobHandler(store *db.DB, cacheDir, device string) jobs.Handler {
	return func(ctx context.Context, job *jobs.Job, report jobs.ReportFunc) (result any, err error) {
		var p exportJobParams
		if err = json.Unmarshal(job.Params, &p); err != nil {
			return nil, fmt.Errorf("export job: invalid params: %w", err)
		}
		if len(p.Items) == 0 {
			return nil, fmt.Errorf("export job: no items")
		}

		out := media.ExportOutputPath(cacheDir, p.ExportID)
		// Cleanup on any failure (including cancellation): a half-written
		// output file and a permanently "processing" export row are both
		// worse than just not having one — GET /api/exports would
		// otherwise show a phantom entry that can never finish or be
		// downloaded.
		defer func() {
			if err == nil {
				return
			}
			if rmErr := os.Remove(out); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Printf("[jobs] export %d: cleanup partial output %s: %v", job.ID, out, rmErr)
			}
			if delErr := store.ExportDelete(context.Background(), p.ExportID); delErr != nil {
				log.Printf("[jobs] export %d: cleanup export row %d: %v", job.ID, p.ExportID, delErr)
			}
		}()

		if err = acquireFFmpegSlot(ctx); err != nil {
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
		if outW <= 0 || outH <= 0 {
			outW, outH = media.ResolveOutputDims(p.Quality, defaultOutputWidth, defaultOutputHeight)
		}

		report(0.05, "planning")
		plan := planExport(ctx, fronts, p.Camera, p.BurnIn)

		cacheExportDir := filepath.Join(cacheDir, "exports")
		tmpDir := filepath.Join(cacheExportDir, "tmp", strconv.FormatInt(job.ID, 10))
		if err = os.MkdirAll(tmpDir, 0o755); err != nil {
			return nil, fmt.Errorf("export job: tmp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		progress := func(_ int64, _ float64) { report(0.5, "encoding") }

		if plan.FastPath {
			if err = runFastPathExport(ctx, fronts, tmpDir, job.ID, out, progress); err != nil {
				return nil, err
			}
		} else {
			if err = runReencodeExport(ctx, store, fronts, tripID, p, outW, outH, getHWMode(), device, tmpDir, job.ID, out, progress); err != nil {
				return nil, err
			}
		}

		info, statErr := os.Stat(out)
		var size int64
		if statErr == nil {
			size = info.Size()
		}

		finalPath, shareIDPtr, expiresAtPtr, ferr := finalizeExportOutput(ctx, store, out, p)
		if ferr != nil {
			err = ferr
			return nil, err
		}
		// Best-effort from here on: ffmpeg already succeeded and
		// finalizeExportOutput already placed the file at finalPath, so a
		// failure persisting that to the exports row is logged rather than
		// treated as a job failure — the deferred cleanup above would
		// otherwise delete a perfectly good finished file over a DB write
		// hiccup.
		if dbErr := store.ExportFinish(ctx, p.ExportID, finalPath, shareIDPtr, size, expiresAtPtr); dbErr != nil {
			log.Printf("[jobs] export %d: finish export row: %v", job.ID, dbErr)
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
		items[i] = media.ReencodeInput{Path: f.path, InS: f.inS, OutS: f.outS}
	}
	spec := media.ReencodeSpec{Items: items, OutW: outW, OutH: outH, Mode: hwMode, Device: device}

	var rears []rearResolvedItem
	if p.Camera == "both" {
		var err error
		rears, err = resolveRearPlacements(ctx, store, fronts, tripID)
		if err != nil {
			return err
		}
		rearItems := make([]media.ReencodeInput, len(rears))
		for i, r := range rears {
			rearItems[i] = media.ReencodeInput{Path: r.path, InS: r.inS, OutS: r.outS}
		}
		spec.Rear = rearItems
	}

	if p.BurnIn {
		if err := applyBurnIn(ctx, store, &spec, fronts, rears, outW, outH, tmpDir); err != nil {
			return err
		}
	}

	script, outLabel, overlayDirs := media.BuildFilterComplex(spec)
	scriptPath := filepath.Join(tmpDir, fmt.Sprintf("%d.filter", jobID))
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		return fmt.Errorf("export job: write filter script: %w", err)
	}

	return media.RunReencode(ctx, spec, overlayDirs, scriptPath, outLabel, out, progress)
}

// applyBurnIn loads detection frames for every item (front, and rear for
// a "both" export), decides once for the whole export whether the
// combined box count fits the brief's drawbox/drawtext filter-script
// method (< media.MaxScriptBoxes) or needs the PNG-overlay-sequence
// method instead, and populates spec.Items[*]/spec.Rear[*] with the
// resulting Overlay or OverlayPNGDir. A single method is chosen for the
// whole export (rather than per item) to keep the ffmpeg filter graph
// simple — mixing two burn-in mechanisms in one graph buys little for a
// typical multi-item export where most items are similarly dense.
func applyBurnIn(ctx context.Context, store *db.DB, spec *media.ReencodeSpec, fronts []resolvedItem, rears []rearResolvedItem, outW, outH int, tmpDir string) error {
	frontFrames := make([][]media.Frame, len(fronts))
	total := 0
	for i, f := range fronts {
		frames, err := loadOverlayFrames(ctx, store, f.info.ID, f.inS, f.outS)
		if err != nil {
			log.Printf("[export] burn-in segment %d: %v", f.info.ID, err)
			continue
		}
		frontFrames[i] = frames
		total += media.CountBoxes(frames)
	}
	rearFrames := make([][]media.Frame, len(rears))
	for i, r := range rears {
		frames, err := loadOverlayFrames(ctx, store, r.segmentID, r.inS, r.outS)
		if err != nil {
			log.Printf("[export] burn-in rear segment %d: %v", r.segmentID, err)
			continue
		}
		rearFrames[i] = frames
		total += media.CountBoxes(frames)
	}
	if total == 0 {
		return nil // nothing detected in the exported ranges; no-op
	}

	if total < media.MaxScriptBoxes {
		fontFile := filepath.Join(tmpDir, "font.ttf")
		if err := media.WriteDefaultFont(fontFile); err != nil {
			return fmt.Errorf("export job: write burn-in font: %w", err)
		}
		for i, frames := range frontFrames {
			if len(frames) > 0 {
				spec.Items[i].Overlay = media.BuildDrawFilters(frames, outW, outH, fontFile)
			}
		}
		for i, frames := range rearFrames {
			if len(frames) > 0 {
				spec.Rear[i].Overlay = media.BuildDrawFilters(frames, outW, outH, fontFile)
			}
		}
		return nil
	}

	log.Printf("[export] burn-in: %d total detection boxes >= %d, rendering PNG overlay sequence(s)", total, media.MaxScriptBoxes)
	for i, frames := range frontFrames {
		if len(frames) == 0 {
			continue
		}
		dir := filepath.Join(tmpDir, fmt.Sprintf("overlay-front-%d", i))
		if _, err := media.RenderOverlayFrames(frames, outW, outH, fronts[i].outS-fronts[i].inS, dir); err != nil {
			return fmt.Errorf("export job: render overlay frames: %w", err)
		}
		spec.Items[i].OverlayPNGDir = dir
	}
	for i, frames := range rearFrames {
		if len(frames) == 0 {
			continue
		}
		dir := filepath.Join(tmpDir, fmt.Sprintf("overlay-rear-%d", i))
		if _, err := media.RenderOverlayFrames(frames, outW, outH, rears[i].outS-rears[i].inS, dir); err != nil {
			return fmt.Errorf("export job: render overlay frames: %w", err)
		}
		spec.Rear[i].OverlayPNGDir = dir
	}
	return nil
}

// rearResolvedItem is one front item's paired rear segment: its id, file
// path, and [inS,outS) trim window, resolved by resolveRearPlacements.
type rearResolvedItem struct {
	segmentID int64
	path      string
	inS, outS float64
}

// resolveRearPlacements resolves, for every front item, the rear segment
// covering the same real-world moment and its corresponding trim window.
//
// It matches by absolute trip-timeline offset (media.ResolveRearSegment
// with pairOffsetS=0), the same approach DualVideoPlayer's rear-slaving
// uses — NOT trip.pair_offset_s: TripCameraPlacements' OffsetS is already
// computed from each segment's own real start_time relative to
// trip.started_at (see its SQL), so it already reflects the rear
// segment's true position on the shared timeline. Shifting by
// pair_offset_s on top of that would double-count the offset and
// misalign the rear clip — the M11 review caught exactly this bug in an
// earlier version of this function, which passed *pair.PairOffsetS here.
func resolveRearPlacements(ctx context.Context, store *db.DB, fronts []resolvedItem, tripID int64) ([]rearResolvedItem, error) {
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

	out := make([]rearResolvedItem, len(fronts))
	for i, f := range fronts {
		frontOffsetS, ok := frontOffsetByID[f.info.ID]
		if !ok {
			return nil, fmt.Errorf("export job: segment %d: no trip placement", f.info.ID)
		}
		mid := frontOffsetS + (f.inS+f.outS)/2
		rearSegID, rearMidLocal, ok := media.ResolveRearSegment(rearPlacements, mid, 0)
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
		out[i] = rearResolvedItem{segmentID: rearSegID, path: rpath, inS: rearIn, outS: rearOut}
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
	// ResolveTargetMount confirms the share actually has sh.MountPoint
	// mounted right now (share.IsMounted, with the allow_local_share_paths
	// test escape hatch) rather than trusting the stored mount_point blindly
	// — the same guard internal/ingest uses before writing anything to a
	// share. sh.MountPoint is already the share's real mount path (not a
	// slug — MountPointFor(base, slug) is for computing it in the first
	// place, at share-creation time), so it must be used directly here, not
	// re-run through MountPointFor.
	targetMount, merr := ingest.ResolveTargetMount(ctx, store, id)
	if merr != nil {
		return "", nil, nil, fmt.Errorf("export job: share target not available: %w", merr)
	}
	destDir := filepath.Join(targetMount, "exports")
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
