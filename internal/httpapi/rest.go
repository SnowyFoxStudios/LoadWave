// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/report"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
)

// handleSnapshot returns the coordinator's whole current state.
//
// A client fetches this once on load and then follows the WebSocket stream,
// which is what lets the live charts append single points rather than redraw
// from a full payload every second.
func (s *Server) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.coord.Snapshot())
}

// handleAgents lists connected agents.
func (s *Server) handleAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"agents": s.coord.Agents()})
}

// handleListRuns lists known runs, newest first.
func (s *Server) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.coord.Runs()})
}

// handleGetRun returns one run's full state.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.coord.RunSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no run %q", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

// handleGetRunConfig returns the exact YAML a run was started from, plus the
// file it came from, if any.
//
// This is what lets the "New run" dialog reopen showing the test that is
// actually active — or that just finished — instead of a blank template: the
// coordinator already keeps the plan's original YAML for workers to
// reconstruct their engine from, so no re-serialisation is needed here.
func (s *Server) handleGetRunConfig(w http.ResponseWriter, r *http.Request) {
	run, ok := s.coord.Lookup(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no run %q", r.PathValue("id"))
		return
	}

	body := map[string]any{
		// A plain string, not the []byte straight off the proto: encoding/json
		// base64-encodes a []byte by default, which is exactly wrong for text
		// a browser is about to show in a textarea.
		"yaml":       string(run.Plan().GetConfigYaml()),
		"sourcePath": run.SourcePath(),
	}

	// The Build form as well as the YAML, so switching to it does not fall
	// back to the blank template. Best-effort: a plan with no embedded
	// configuration (assembled purely from CLI flags, with no file and no
	// scenario at all) yields nothing to convert, and the dialog keeps its
	// blank draft rather than showing an error over something this minor.
	if cfg, err := scenario.FromPlan(run.Plan()); err == nil {
		body["draft"] = draftFromConfig(cfg)
	}

	writeJSON(w, http.StatusOK, body)
}

// handleSaveRunConfig writes an edited configuration back to the file a run
// was started from.
//
// Restricted to runs that have one: a configuration built from a Go scenario,
// from CLI flags with no file, or submitted to the dashboard directly has
// nowhere on disk that "save" could mean.
func (s *Server) handleSaveRunConfig(w http.ResponseWriter, r *http.Request) {
	if s.guardReadOnly(w) {
		return
	}

	run, ok := s.coord.Lookup(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no run %q", r.PathValue("id"))
		return
	}
	path := run.SourcePath()
	if path == "" {
		writeError(w, http.StatusConflict, "this run has no source file to save to")
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	// Parsed, not just stored: writing YAML the parser would reject leaves a
	// file on disk that the next `loadwave run` cannot read, which is a worse
	// outcome than declining the save and saying why.
	if _, err := scenario.Parse(body); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	if err := os.WriteFile(path, body, 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, "write %s: %s", path, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"savedTo": path})
}

// handleRunSeries returns a run's cumulative per-series aggregates, which is
// what the endpoint breakdown table renders.
func (s *Server) handleRunSeries(w http.ResponseWriter, r *http.Request) {
	run, ok := s.coord.Lookup(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no run %q", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": run.Store().Summary()})
}

// handleRunReport renders a run as a self-contained HTML file.
//
// Served as a download rather than a page: this is an artefact to keep, attach
// to a ticket or archive alongside a release, not another view of the live
// dashboard.
func (s *Server) handleRunReport(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.coord.RunSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no run %q", r.PathValue("id"))
		return
	}

	data, err := report.Build(snapshot, time.Now())
	if err != nil {
		writeError(w, http.StatusConflict, "%s", err.Error())
		return
	}

	// Rendered into a buffer first: an error halfway through a template would
	// otherwise leave a truncated file that looks like a complete one.
	var body bytes.Buffer
	if err := report.Render(&body, data); err != nil {
		s.log.Error("could not render the report", "run", r.PathValue("id"), "error", err)
		writeError(w, http.StatusInternalServerError, "could not render the report")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", report.Filename(*snapshot.Run)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

// handleStartRun accepts a test configuration and begins a run.
//
// The body is a LoadWave configuration in either YAML or JSON. Both are
// accepted from the same parser because JSON is a subset of YAML, which keeps
// the browser posting the object it already has while a CI script can send the
// very file it keeps in the repository.
func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	if s.guardReadOnly(w) {
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body is empty; send a test configuration")
		return
	}

	cfg, err := scenario.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	// A configuration posted to the dashboard is never file-backed, even if
	// it started life as a file: only the browser's in-memory text exists at
	// this point, so there is nothing on disk to save further edits back to.
	run, err := s.coord.StartRun(cfg, "")
	if err != nil {
		// A rejected start is the operator's problem to fix — no agents, or a
		// run already going — rather than a server fault.
		writeError(w, http.StatusConflict, "%s", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"runId": run.ID()})
}

// stopRequest is the body of a stop call.
type stopRequest struct {
	// Graceful lets in-flight iterations finish. Defaults to true, because an
	// abrupt stop manufactures a cliff of cancelled requests that then show
	// up as failures in the very results the operator is about to read.
	Graceful *bool  `json:"graceful"`
	Reason   string `json:"reason"`
}

// handleStopRun ends a run.
func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	if s.guardReadOnly(w) {
		return
	}

	req := stopRequest{}
	if body, err := readBody(r); err == nil && len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err.Error())
			return
		}
	}

	graceful := true
	if req.Graceful != nil {
		graceful = *req.Graceful
	}

	if err := s.coord.StopRun(r.PathValue("id"), graceful, req.Reason); err != nil {
		writeError(w, http.StatusConflict, "%s", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"stopping": r.PathValue("id")})
}

// scaleRequest is the body of a scale call.
type scaleRequest struct {
	// VUs is the new peak virtual user count. The profile's shape is kept and
	// rescaled to this ceiling.
	VUs int `json:"vus"`

	// RampSeconds spreads the change over a period rather than applying it in
	// one tick. Zero is immediate.
	RampSeconds float64 `json:"rampSeconds"`
}

// handleScaleRun changes a running test's peak virtual user count.
func (s *Server) handleScaleRun(w http.ResponseWriter, r *http.Request) {
	if s.guardReadOnly(w) {
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}

	var req scaleRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: %s", err.Error())
		return
	}
	if req.VUs < 0 {
		writeError(w, http.StatusBadRequest, "vus cannot be negative")
		return
	}
	if req.RampSeconds < 0 {
		writeError(w, http.StatusBadRequest, "rampSeconds cannot be negative")
		return
	}

	ramp := time.Duration(req.RampSeconds * float64(time.Second))
	if err := s.coord.ScaleRun(r.PathValue("id"), req.VUs, ramp); err != nil {
		writeError(w, http.StatusConflict, "%s", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"vus":         req.VUs,
		"rampSeconds": req.RampSeconds,
	})
}

// uiUnavailable explains a binary built without the dashboard assets.
const uiUnavailable = `LoadWave: the dashboard has not been built into this binary.

The REST API is available at /api/v1/. To build the UI:

    make ui

or, without make:

    npm --prefix web ci
    npm --prefix web run build

then rebuild the binary.
`

// handleUI serves the embedded single-page application.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.hasUI {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(uiUnavailable))
		return
	}

	// An unmatched /api/ path is a client error, not a request for the SPA.
	// Falling through to index.html would hand a script a page of HTML where
	// it expected JSON, which is a miserable thing to debug.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusNotFound, "no such endpoint: %s", r.URL.Path)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	file, err := s.assets.Open(name)
	if err != nil {
		// Any other path belongs to the client-side router, which needs the
		// application shell in order to resolve it.
		s.serveIndex(w, r)
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		s.serveIndex(w, r)
		return
	}

	// Vite fingerprints every asset filename, so those are immutable and can
	// be cached hard. index.html must not be, or a redeploy would leave
	// browsers pinned to a stale shell referencing assets that no longer
	// exist.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	seeker, ok := file.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		s.serveIndex(w, r)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), seeker)
}

// serveIndex sends the application shell.
func (s *Server) serveIndex(w http.ResponseWriter, _ *http.Request) {
	data, err := fs.ReadFile(s.assets, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard shell is missing")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
