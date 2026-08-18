// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
)

// WebSocket tuning.
const (
	// writeTimeout bounds a single frame write. A browser tab that has been
	// suspended stops reading, and without this the coordinator would hold
	// the connection and its buffers indefinitely.
	writeTimeout = 10 * time.Second

	// pingInterval keeps intermediaries from closing an idle connection
	// during the quiet stretch between runs.
	pingInterval = 25 * time.Second

	// snapshotType is the first frame sent on a new connection.
	snapshotType = "snapshot"
)

// frame is one message on the wire.
//
// The snapshot and the incremental updates are sent through one envelope with
// a discriminating type, so the client has a single message handler rather
// than a connection state machine.
type frame struct {
	Type     string                `json:"type"`
	Snapshot *coordinator.Snapshot `json:"snapshot,omitempty"`
	*coordinator.Update
}

// handleStream upgrades to a WebSocket and streams live updates.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.cfg.AllowedOrigins,
		// Updates are small JSON objects that compress well, and a live
		// dashboard sends one every second for the length of a run.
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.log.Debug("websocket upgrade failed", "error", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub := s.coord.Subscribe()
	defer sub.Close()

	// The initial snapshot goes out before any update, so a client that
	// connects mid-run sees the history it missed rather than starting its
	// charts from wherever the next tick happens to land.
	snapshot := s.coord.Snapshot()
	if err := s.writeFrame(ctx, conn, frame{Type: snapshotType, Snapshot: &snapshot}); err != nil {
		return
	}

	// Reading is what surfaces a closed connection and honours a client's
	// close frame; the protocol itself is one-directional.
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-sub.Done():
			_ = conn.Close(websocket.StatusGoingAway, "coordinator shutting down")
			return

		case update := <-sub.Updates():
			if err := s.writeFrame(ctx, conn, frame{Type: update.Type, Update: &update}); err != nil {
				s.log.Debug("dropping websocket client", "error", err)
				return
			}

		case <-ping.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				return
			}
		}
	}
}

// writeFrame sends one message with a bounded write deadline.
func (s *Server) writeFrame(ctx context.Context, conn *websocket.Conn, msg frame) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	err := wsjson.Write(writeCtx, conn, msg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return err
}
