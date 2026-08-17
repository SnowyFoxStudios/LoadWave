// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/durationpb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
)

// ErrSessionClosed is returned by Session.Send once the node has gone.
var ErrSessionClosed = errors.New("control session is closed")

// Session is a supervisor's handle on one connected node.
//
// It is safe for concurrent use, and remains usable until the node
// disconnects, at which point every Send fails with ErrSessionClosed.
type Session struct {
	// ID is the node's self-declared identifier. Reconnections reuse it,
	// which is how a supervisor recognises a returning node rather than
	// treating it as a new one.
	ID string

	// Hello is what the node advertised on its most recent connection.
	Hello *loadwavev1.NodeHello

	// RemoteAddr is the peer address, for operator-facing display.
	RemoteAddr string

	// JoinedAt is when this particular connection was established.
	JoinedAt time.Time

	out    chan *loadwavev1.NodeDown
	closed atomic.Bool
	done   chan struct{}

	lastSeen atomic.Int64
	dropped  atomic.Uint64
}

// Send queues a command for the node.
//
// Commands, unlike telemetry, matter: dropping a StopRun would leave a node
// generating load nobody asked for. So a full queue is an error the caller
// must deal with, rather than something silently swallowed. In practice the
// queue only fills when a node has stopped reading, which the stream will
// shortly report anyway.
func (s *Session) Send(msg *loadwavev1.NodeDown) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	select {
	case s.out <- msg:
		return nil
	case <-s.done:
		return ErrSessionClosed
	default:
		s.dropped.Add(1)
		return fmt.Errorf("control queue for node %q is full", s.ID)
	}
}

// LastSeen reports when the node last sent anything.
func (s *Session) LastSeen() time.Time { return time.Unix(0, s.lastSeen.Load()) }

// Close disconnects the node.
func (s *Session) Close() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
	}
}

// Closed reports whether the session has ended.
func (s *Session) Closed() bool { return s.closed.Load() }

// SessionHandler receives everything a connected node does.
//
// Every method is called from that node's own receive goroutine, so
// implementations must be safe for concurrent use across sessions, and must
// not block for long.
type SessionHandler interface {
	// OnJoin is called once the node has identified itself. Returning an
	// error rejects the connection, and the node will retry.
	OnJoin(ctx context.Context, session *Session) error

	// OnLeave is called exactly once per accepted session, when it ends.
	OnLeave(session *Session)

	OnHeartbeat(session *Session, beat *loadwavev1.NodeHeartbeat)
	OnMetrics(session *Session, batch *loadwavev1.MetricBatch)
	OnRunStatus(session *Session, update *loadwavev1.RunStatusUpdate)
	OnLog(session *Session, event *loadwavev1.LogEvent)
}

// ServerConfig configures a supervisor's end of the protocol.
type ServerConfig struct {
	Handler SessionHandler
	Logger  *slog.Logger

	// Version is reported to nodes so they can warn about a mismatch.
	Version string

	// HeartbeatInterval is how often nodes should report liveness.
	HeartbeatInterval time.Duration

	// MetricsInterval is how often nodes should flush metric batches. It
	// doubles as the bucket width the coordinator's store expects.
	MetricsInterval time.Duration

	// QueueSize bounds each session's outbound command buffer.
	QueueSize int
}

// Defaults applied to a zero ServerConfig.
const (
	DefaultServerHeartbeatInterval = 2 * time.Second
	DefaultMetricsInterval         = time.Second
	DefaultServerQueueSize         = 64
)

func (c *ServerConfig) applyDefaults() {
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultServerHeartbeatInterval
	}
	if c.MetricsInterval <= 0 {
		c.MetricsInterval = DefaultMetricsInterval
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultServerQueueSize
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Server implements the ControlService for one supervisor tier.
type Server struct {
	loadwavev1.UnimplementedControlServiceServer

	cfg ServerConfig
	log *slog.Logger
}

// NewServer prepares a supervisor endpoint.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Handler == nil {
		return nil, errors.New("control server needs a session handler")
	}
	cfg.applyDefaults()
	return &Server{cfg: cfg, log: cfg.Logger}, nil
}

// Join implements loadwavev1.ControlServiceServer.
func (s *Server) Join(stream loadwavev1.ControlService_JoinServer) error {
	ctx := stream.Context()

	hello, err := s.awaitHello(stream)
	if err != nil {
		return err
	}

	session := &Session{
		ID:         hello.GetNodeId(),
		Hello:      hello,
		RemoteAddr: peerAddr(ctx),
		JoinedAt:   time.Now(),
		out:        make(chan *loadwavev1.NodeDown, s.cfg.QueueSize),
		done:       make(chan struct{}),
	}
	session.lastSeen.Store(time.Now().UnixNano())

	log := s.log.With("node", session.ID, "peer", session.RemoteAddr)

	if err := s.cfg.Handler.OnJoin(ctx, session); err != nil {
		log.Warn("rejected node", "error", err)
		return fmt.Errorf("join rejected: %w", err)
	}
	defer s.cfg.Handler.OnLeave(session)
	defer session.Close()

	accepted := &loadwavev1.NodeDown{
		Payload: &loadwavev1.NodeDown_Accepted{
			Accepted: &loadwavev1.Accepted{
				AssignedNodeId:    session.ID,
				HeartbeatInterval: durationpb.New(s.cfg.HeartbeatInterval),
				MetricsInterval:   durationpb.New(s.cfg.MetricsInterval),
				SupervisorVersion: s.cfg.Version,
			},
		},
	}
	if err := stream.Send(accepted); err != nil {
		return fmt.Errorf("send accepted: %w", err)
	}

	log.Info("node joined",
		"hostname", hello.GetHostname(),
		"version", hello.GetVersion(),
		"cores", hello.GetCpuCores(),
		"maxWorkers", hello.GetMaxWorkers(),
		"maxVUs", hello.GetMaxVus())

	// The send pump runs alongside the receive loop, which owns the session's
	// lifetime. OnJoin has already run, so the handler may push commands from
	// the moment it returns and they will be delivered here.
	sendErr := make(chan error, 1)
	go func() { sendErr <- s.sendLoop(stream, session) }()

	recvErr := s.receiveLoop(stream, session)
	session.Close()
	<-sendErr

	if recvErr != nil {
		log.Info("node disconnected", "error", recvErr)
	} else {
		log.Info("node disconnected")
	}
	return recvErr
}

// awaitHello reads the first message, which must identify the node.
func (s *Server) awaitHello(stream loadwavev1.ControlService_JoinServer) (*loadwavev1.NodeHello, error) {
	first, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("receive hello: %w", err)
	}

	hello, ok := first.GetPayload().(*loadwavev1.NodeUp_Hello)
	if !ok {
		return nil, fmt.Errorf("first message was %T, expected a hello", first.GetPayload())
	}
	if hello.Hello.GetNodeId() == "" {
		return nil, errors.New("hello has an empty node id")
	}
	return hello.Hello, nil
}

// sendLoop delivers queued commands to the node.
func (s *Server) sendLoop(stream loadwavev1.ControlService_JoinServer, session *Session) error {
	for {
		select {
		case <-session.done:
			return nil
		case <-stream.Context().Done():
			return nil
		case msg := <-session.out:
			if err := stream.Send(msg); err != nil {
				session.Close()
				return err
			}
		}
	}
}

// receiveLoop dispatches everything the node sends up.
func (s *Server) receiveLoop(stream loadwavev1.ControlService_JoinServer, session *Session) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		session.lastSeen.Store(time.Now().UnixNano())

		switch payload := msg.GetPayload().(type) {
		case *loadwavev1.NodeUp_Heartbeat:
			s.cfg.Handler.OnHeartbeat(session, payload.Heartbeat)
		case *loadwavev1.NodeUp_Metrics:
			s.cfg.Handler.OnMetrics(session, payload.Metrics)
		case *loadwavev1.NodeUp_RunStatus:
			s.cfg.Handler.OnRunStatus(session, payload.RunStatus)
		case *loadwavev1.NodeUp_Log:
			s.cfg.Handler.OnLog(session, payload.Log)
		case *loadwavev1.NodeUp_Pong:
			// lastSeen has already been refreshed, which is the whole point.
		case *loadwavev1.NodeUp_Hello:
			// A second hello on a live stream is harmless; take the update.
			session.Hello = payload.Hello
		default:
			s.log.Warn("unrecognised message from node", "node", session.ID, "type", fmt.Sprintf("%T", payload))
		}
	}
}

// peerAddr extracts the caller's address for display.
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

// SessionRegistry tracks the nodes currently connected to a supervisor.
//
// It is the piece that makes reconnection work: a node that drops and comes
// back reuses its id, and the registry replaces the stale session rather than
// accumulating ghosts. Both the coordinator and the agent embed one.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewSessionRegistry returns an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{sessions: make(map[string]*Session)}
}

// Add registers a session, displacing and closing any previous one with the
// same node id. It reports the displaced session, if there was one.
func (r *SessionRegistry) Add(session *Session) *Session {
	r.mu.Lock()
	previous := r.sessions[session.ID]
	r.sessions[session.ID] = session
	r.mu.Unlock()

	if previous != nil {
		previous.Close()
	}
	return previous
}

// Remove deregisters a session, but only if it is still the current one for
// its node id.
//
// The guard matters during a reconnection race: the new session can be
// registered before the old one's goroutine has finished unwinding, and an
// unguarded delete would evict the live session on behalf of the dead one.
func (r *SessionRegistry) Remove(session *Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if current, ok := r.sessions[session.ID]; ok && current == session {
		delete(r.sessions, session.ID)
		return true
	}
	return false
}

// Get returns the current session for a node id.
func (r *SessionRegistry) Get(id string) (*Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	session, ok := r.sessions[id]
	return session, ok
}

// All returns every live session.
func (r *SessionRegistry) All() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*Session, 0, len(r.sessions))
	for _, session := range r.sessions {
		out = append(out, session)
	}
	return out
}

// Len reports how many nodes are connected.
func (r *SessionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// Broadcast sends a command to every connected node, returning the first
// error. Delivery to the remaining nodes is attempted regardless: a run must
// still stop on nine agents when the tenth is unreachable.
func (r *SessionRegistry) Broadcast(msg *loadwavev1.NodeDown) error {
	var firstErr error
	for _, session := range r.All() {
		if err := session.Send(msg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
