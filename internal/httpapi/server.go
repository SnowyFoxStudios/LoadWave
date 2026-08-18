// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package httpapi serves the LoadWave dashboard and its REST and WebSocket
// interfaces.
//
// The same surface backs the browser UI and any script that wants to drive a
// run from CI, which is deliberate: anything the dashboard can do should be
// automatable without it.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/buildinfo"
	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
	"github.com/SnowyFoxStudios/LoadWave/web"
)

// Config describes the dashboard server.
type Config struct {
	// Addr is the listen address, such as ":8088".
	Addr string

	// Coordinator supplies the data and receives the commands.
	Coordinator *coordinator.Coordinator

	Logger *slog.Logger

	// AllowedOrigins lists the browser origins permitted to open a WebSocket.
	// Empty means same-origin only, which is what a normal deployment wants;
	// the frontend dev server needs its own origin added.
	AllowedOrigins []string

	// ReadOnly refuses every mutating request. Useful when exposing a live
	// run to an audience who should not be able to stop it.
	ReadOnly bool

	// OnShutdown ends the whole process, if the deployment allows it.
	//
	// Separate from stopping a run on purpose. Stopping a run has to leave
	// the coordinator up — the operator still wants the results, the report,
	// and usually another run — so shutting down needs its own deliberate
	// action rather than being a side effect of the last run ending.
	//
	// Nil disables the endpoint.
	OnShutdown func(reason string)

	// Registry holds the scenarios compiled into this binary.
	//
	// Needed so that validation can tell a configuration referring to a Go
	// scenario this binary does not have from one that is merely misspelled —
	// a distinction the scenario builder has no other way to make.
	Registry *loadwave.Registry
}

// Timeouts. Read and idle timeouts are generous because the WebSocket
// endpoint holds connections open for the length of a run; the write timeout
// is deliberately absent for the same reason and is enforced per-response
// instead.
const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Server serves the dashboard and API.
type Server struct {
	cfg    Config
	log    *slog.Logger
	coord  *coordinator.Coordinator
	http   *http.Server
	addr   string
	ready  chan struct{}
	assets fs.FS
	hasUI  bool
}

// New prepares the server. It does not listen; Run does that.
func New(cfg Config) (*Server, error) {
	if cfg.Coordinator == nil {
		return nil, errors.New("dashboard server needs a coordinator")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8088"
	}

	assets, hasUI := web.Assets()
	s := &Server{
		cfg:    cfg,
		log:    cfg.Logger.With("component", "dashboard"),
		coord:  cfg.Coordinator,
		ready:  make(chan struct{}),
		assets: assets,
		hasUI:  hasUI,
	}
	if !hasUI {
		s.log.Warn("dashboard assets are not built; the API is available but the UI is not. " +
			"Run `make ui` to build it")
	}

	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// routes builds the request multiplexer.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/version", s.handleVersion)
	mux.HandleFunc("GET /api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/v1/agents", s.handleAgents)
	mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	mux.HandleFunc("POST /api/v1/runs", s.handleStartRun)
	mux.HandleFunc("POST /api/v1/validate", s.handleValidate)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/v1/runs/{id}/config", s.handleGetRunConfig)
	mux.HandleFunc("PUT /api/v1/runs/{id}/config", s.handleSaveRunConfig)
	mux.HandleFunc("POST /api/v1/runs/{id}/stop", s.handleStopRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/scale", s.handleScaleRun)
	mux.HandleFunc("GET /api/v1/runs/{id}/series", s.handleRunSeries)
	mux.HandleFunc("GET /api/v1/runs/{id}/report.html", s.handleRunReport)
	mux.HandleFunc("POST /api/v1/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	mux.HandleFunc("/", s.handleUI)

	return s.withRecovery(s.withLogging(mux))
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	s.addr = listener.Addr().String()
	close(s.ready)

	s.log.Info("dashboard listening", "url", "http://"+displayAddr(s.addr), "ui", s.hasUI)

	serveErr := make(chan error, 1)
	go func() {
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return s.http.Close()
		}
		return nil
	}
}

// Addr returns the bound address once the listener is up.
func (s *Server) Addr() string {
	<-s.ready
	return s.addr
}

// URL returns the dashboard's browsable address.
func (s *Server) URL() string { return "http://" + displayAddr(s.Addr()) }

// displayAddr turns a wildcard bind address into something clickable.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		return net.JoinHostPort("localhost", port)
	}
	return addr
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer, which the
// WebSocket upgrade needs in order to hijack the connection.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// withLogging records API requests at debug level.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets and the long-lived stream would drown the log without
		// telling anyone anything useful.
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/v1/stream" {
			next.ServeHTTP(w, r)
			return
		}

		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		s.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration", time.Since(started).Round(time.Millisecond))
	})
}

// withRecovery turns a panic in a handler into a 500 instead of a dead server.
//
// The dashboard is frequently the only window an operator has onto a run that
// is already going wrong; a nil dereference while rendering it should not also
// cost them the run.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic while handling a request",
					"path", r.URL.Path, "panic", recovered)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

// errorBody is the shape of every error response.
type errorBody struct {
	Error string `json:"error"`
}

// writeJSON sends a value as JSON.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already committed, so there is nothing useful
		// left to say to the client.
		return
	}
}

// writeError sends a JSON error.
func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, errorBody{Error: fmt.Sprintf(format, args...)})
}

// maxRequestBody caps how much a client may send. Test configurations are
// kilobytes; anything much larger is a mistake or an attack.
const maxRequestBody = 1 << 20

// readBody reads a request body under the size cap.
func readBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(data) > maxRequestBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxRequestBody)
	}
	return data, nil
}

// guardReadOnly rejects a mutating request when the server is read-only,
// reporting whether the caller should stop.
func (s *Server) guardReadOnly(w http.ResponseWriter) bool {
	if !s.cfg.ReadOnly {
		return false
	}
	writeError(w, http.StatusForbidden, "this dashboard is read-only")
	return true
}

// buildInfo is exposed at /api/v1/version.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

// handleHealth reports that the coordinator is up.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"agents":      len(s.coord.Agents()),
		"canShutDown": s.cfg.OnShutdown != nil && !s.cfg.ReadOnly,
	})
}

// handleShutdown ends the process.
func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	if s.guardReadOnly(w) {
		return
	}
	if s.cfg.OnShutdown == nil {
		writeError(w, http.StatusNotImplemented, "this deployment cannot be shut down from the dashboard")
		return
	}

	// Answered before shutting down, so the browser learns it worked rather
	// than seeing the connection drop and assuming a crash.
	writeJSON(w, http.StatusAccepted, map[string]any{"shuttingDown": true})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	s.log.Info("shutdown requested from the dashboard")
	go s.cfg.OnShutdown("powered off from the dashboard")
}
