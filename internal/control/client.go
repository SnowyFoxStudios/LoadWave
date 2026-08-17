// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package control carries commands down and telemetry up between the tiers of
// a LoadWave cluster.
//
// Both tiers speak the same protocol: an agent joining a coordinator over TCP
// and a worker process joining its agent over a Unix socket use the identical
// client and server here. The node always dials, and holds one long-lived
// bidirectional stream open for as long as it is alive. That direction is a
// deliberate choice — it lets agents live behind NAT with no inbound ports,
// and it makes a broken stream an unambiguous liveness signal instead of
// something that has to be inferred from timeouts.
package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
)

// Handler receives the commands a supervisor sends down the stream.
//
// Calls are made from the client's receive goroutine, one at a time and in
// order. An implementation that needs to do something slow should hand off to
// its own goroutine rather than block, since a stalled handler stops the node
// noticing anything else — including a stop command.
type Handler interface {
	OnAccepted(ctx context.Context, msg *loadwavev1.Accepted) error
	OnStartRun(ctx context.Context, msg *loadwavev1.StartRun) error
	OnSetQuota(ctx context.Context, msg *loadwavev1.SetQuota) error
	OnStopRun(ctx context.Context, msg *loadwavev1.StopRun) error
}

// ClientConfig configures a node's connection to its supervisor.
type ClientConfig struct {
	// Target is a gRPC target: "host:port" for a coordinator, or
	// "unix:///path/to.sock" for a local agent.
	Target string

	// Hello advertises this node's identity and capacity. It is re-sent on
	// every reconnection, so a supervisor always has current information.
	Hello *loadwavev1.NodeHello

	// Handler receives downstream commands.
	Handler Handler

	// Heartbeat supplies the current statistics each time a heartbeat is due.
	// Optional; without it, heartbeats carry only a sequence number.
	Heartbeat func() *loadwavev1.NodeHeartbeat

	Logger *slog.Logger

	// QueueSize bounds the upstream buffer. When it fills, telemetry is
	// dropped rather than allowed to block the caller: a load generator must
	// not slow down because its reporting channel is congested. Zero applies
	// DefaultQueueSize.
	QueueSize int

	// ReconnectMin and ReconnectMax bound the backoff between attempts.
	ReconnectMin time.Duration
	ReconnectMax time.Duration

	// DialOptions are appended to the defaults, for TLS and interceptors.
	DialOptions []grpc.DialOption
}

// Defaults applied to a zero ClientConfig.
const (
	DefaultQueueSize    = 1024
	DefaultReconnectMin = 250 * time.Millisecond
	DefaultReconnectMax = 15 * time.Second
	// DefaultHeartbeatInterval is used until the supervisor says otherwise.
	DefaultHeartbeatInterval = 2 * time.Second
)

func (c *ClientConfig) applyDefaults() {
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.ReconnectMin <= 0 {
		c.ReconnectMin = DefaultReconnectMin
	}
	if c.ReconnectMax < c.ReconnectMin {
		c.ReconnectMax = max(DefaultReconnectMax, c.ReconnectMin)
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Client maintains a node's control stream, reconnecting as needed.
type Client struct {
	cfg ClientConfig
	log *slog.Logger

	out chan *loadwavev1.NodeUp

	connected    atomic.Bool
	dropped      atomic.Uint64
	sequence     atomic.Int64
	heartbeatDur atomic.Int64
}

// NewClient validates the configuration and prepares a client. It does not
// connect; Run does that.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Target == "" {
		return nil, errors.New("control client needs a target")
	}
	if cfg.Hello == nil || cfg.Hello.GetNodeId() == "" {
		return nil, errors.New("control client needs a hello with a node id")
	}
	if cfg.Handler == nil {
		return nil, errors.New("control client needs a handler")
	}
	cfg.applyDefaults()

	c := &Client{
		cfg: cfg,
		log: cfg.Logger.With("node", cfg.Hello.GetNodeId(), "target", cfg.Target),
		out: make(chan *loadwavev1.NodeUp, cfg.QueueSize),
	}
	c.heartbeatDur.Store(int64(DefaultHeartbeatInterval))
	return c, nil
}

// Connected reports whether a stream is currently established.
func (c *Client) Connected() bool { return c.connected.Load() }

// Dropped reports how many upstream messages were discarded because the queue
// was full.
func (c *Client) Dropped() uint64 { return c.dropped.Load() }

// Send queues a message for the supervisor, reporting whether it was accepted.
//
// It never blocks. Telemetry is worth less than throughput, so a full queue
// discards the message and increments the drop counter, which is reported in
// the next batch and surfaced in the dashboard. Nothing here is load-bearing
// for correctness — the supervisor tolerates gaps by design.
func (c *Client) Send(msg *loadwavev1.NodeUp) bool {
	select {
	case c.out <- msg:
		return true
	default:
		c.dropped.Add(1)
		return false
	}
}

// SendMetrics queues a metric batch.
func (c *Client) SendMetrics(batch *loadwavev1.MetricBatch) bool {
	return c.Send(&loadwavev1.NodeUp{Payload: &loadwavev1.NodeUp_Metrics{Metrics: batch}})
}

// SendRunStatus queues a run status update.
func (c *Client) SendRunStatus(update *loadwavev1.RunStatusUpdate) bool {
	return c.Send(&loadwavev1.NodeUp{Payload: &loadwavev1.NodeUp_RunStatus{RunStatus: update}})
}

// SendLog queues a log event.
func (c *Client) SendLog(event *loadwavev1.LogEvent) bool {
	return c.Send(&loadwavev1.NodeUp{Payload: &loadwavev1.NodeUp_Log{Log: event}})
}

// Run connects and keeps the stream alive until ctx is cancelled.
//
// It returns nil on cancellation. Any other error means the client gave up,
// which currently only happens if the target cannot be parsed at all.
func (c *Client) Run(ctx context.Context) error {
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, c.cfg.DialOptions...)

	conn, err := grpc.NewClient(c.cfg.Target, opts...)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.cfg.Target, err)
	}
	defer func() { _ = conn.Close() }()

	client := loadwavev1.NewControlServiceClient(conn)
	backoff := c.cfg.ReconnectMin

	for {
		if ctx.Err() != nil {
			return nil
		}

		err := c.session(ctx, client)
		c.connected.Store(false)

		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			c.log.Warn("control stream lost, reconnecting", "error", err, "in", backoff)
		default:
			c.log.Info("control stream closed by supervisor, reconnecting", "in", backoff)
		}

		if !sleepCtx(ctx, jitter(backoff)) {
			return nil
		}
		backoff = min(backoff*2, c.cfg.ReconnectMax)
	}
}

// session runs one connection attempt from hello to disconnection.
func (c *Client) session(ctx context.Context, client loadwavev1.ControlServiceClient) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.Join(sessionCtx)
	if err != nil {
		return fmt.Errorf("open control stream: %w", err)
	}

	hello := &loadwavev1.NodeUp{Payload: &loadwavev1.NodeUp_Hello{Hello: c.cfg.Hello}}
	if err := stream.Send(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	c.connected.Store(true)
	c.log.Info("joined supervisor")

	// The receive loop owns the session's lifetime: when it returns, the
	// stream is finished one way or another, and cancelling brings the send
	// and heartbeat loops down with it.
	sendDone := make(chan struct{})
	go func() { defer close(sendDone); _ = c.sendLoop(sessionCtx, stream) }()

	beatDone := make(chan struct{})
	go func() { defer close(beatDone); c.heartbeatLoop(sessionCtx) }()

	err = c.receiveLoop(sessionCtx, stream)
	cancel()
	<-sendDone
	<-beatDone
	return err
}

// receiveLoop dispatches downstream commands until the stream ends.
func (c *Client) receiveLoop(ctx context.Context, stream grpc.BidiStreamingClient[loadwavev1.NodeUp, loadwavev1.NodeDown]) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := c.dispatch(ctx, msg); err != nil {
			c.log.Error("failed to handle supervisor command", "error", err)
		}
	}
}

// dispatch routes one downstream message to the handler.
func (c *Client) dispatch(ctx context.Context, msg *loadwavev1.NodeDown) error {
	switch payload := msg.GetPayload().(type) {
	case *loadwavev1.NodeDown_Accepted:
		if interval := payload.Accepted.GetHeartbeatInterval().AsDuration(); interval > 0 {
			c.heartbeatDur.Store(int64(interval))
		}
		return c.cfg.Handler.OnAccepted(ctx, payload.Accepted)

	case *loadwavev1.NodeDown_StartRun:
		return c.cfg.Handler.OnStartRun(ctx, payload.StartRun)

	case *loadwavev1.NodeDown_SetQuota:
		return c.cfg.Handler.OnSetQuota(ctx, payload.SetQuota)

	case *loadwavev1.NodeDown_StopRun:
		return c.cfg.Handler.OnStopRun(ctx, payload.StopRun)

	case *loadwavev1.NodeDown_Ping:
		// Answered here rather than in the handler: liveness is the
		// transport's business, and a node whose handler is wedged should
		// still look wedged rather than healthy.
		pong := &loadwavev1.Pong{
			Nonce:  payload.Ping.GetNonce(),
			SentAt: timestamppb.Now(),
		}
		c.Send(&loadwavev1.NodeUp{Payload: &loadwavev1.NodeUp_Pong{Pong: pong}})
		return nil

	default:
		return fmt.Errorf("unrecognised command %T", payload)
	}
}

// sendLoop drains the outbound queue onto the stream.
func (c *Client) sendLoop(ctx context.Context, stream grpc.BidiStreamingClient[loadwavev1.NodeUp, loadwavev1.NodeDown]) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-c.out:
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// heartbeatLoop reports liveness and load on the interval the supervisor asked
// for.
func (c *Client) heartbeatLoop(ctx context.Context) {
	for {
		interval := time.Duration(c.heartbeatDur.Load())
		timer := time.NewTimer(interval)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		beat := &loadwavev1.NodeHeartbeat{}
		if c.cfg.Heartbeat != nil {
			if supplied := c.cfg.Heartbeat(); supplied != nil {
				beat = supplied
			}
		}
		beat.Sequence = c.sequence.Add(1)
		beat.SentAt = timestamppb.Now()

		c.Send(&loadwavev1.NodeUp{Payload: &loadwavev1.NodeUp_Heartbeat{Heartbeat: beat}})
	}
}

// jitter spreads reconnection attempts so that a fleet which lost its
// coordinator together does not come back in lockstep and knock it over again.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

// sleepCtx waits for d, reporting false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
