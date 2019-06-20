package gateway

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spec-tacles/gateway/compression"
	"github.com/spec-tacles/go/types"
)

// Shard represents a Gateway shard
type Shard struct {
	Gateway *types.GatewayBot
	conn    *Connection

	opts      *ShardOptions
	limiter   *limiter
	reopening atomic.Value
	packets   *sync.Pool

	connMu sync.Mutex

	sessionID string
	acks      chan struct{}
	seq       *uint64
}

// NewShard creates a new Gateway shard
func NewShard(opts *ShardOptions) *Shard {
	opts.init()

	return &Shard{
		opts:    opts,
		limiter: newLimiter(120, time.Minute),
		packets: &sync.Pool{
			New: func() interface{} {
				return new(types.ReceivePacket)
			},
		},
		seq:  new(uint64),
		acks: make(chan struct{}),
	}
}

// Open starts a new session
func (s *Shard) Open() (err error) {
	err = s.connect()
	for s.handleClose(err) {
		err = s.connect()
	}
	return err
}

// connect runs a single websocket connection; errors may indicate the connection is recoverable
func (s *Shard) connect() (err error) {
	if s.Gateway == nil {
		return ErrGatewayAbsent
	}

	url := s.gatewayURL()
	s.log(LogLevelInfo, "Connecting using URL: %s", url)

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return
	}
	s.conn = NewConnection(conn, compression.NewZstd())

	stop := make(chan struct{}, 0)
	defer close(stop)

	err = s.expectPacket(types.GatewayOpHello, types.GatewayEventNone, s.handleHello(stop))
	if err != nil {
		return
	}

	if s.sessionID == "" && atomic.LoadUint64(s.seq) == 0 {
		if err = s.sendIdentify(); err != nil {
			return
		}

		s.log(LogLevelDebug, "Sent identify upon connecting")

		err = s.expectPacket(types.GatewayOpDispatch, types.GatewayEventReady, nil)
		if err != nil {
			return
		}

		s.log(LogLevelInfo, "received ready event")
	} else {
		if err = s.sendResume(); err != nil {
			return
		}

		s.log(LogLevelDebug, "Sent resume upon connecting")
	}

	s.log(LogLevelDebug, "beginning normal message consumption")
	for {
		err = s.readPacket(nil)
		if err != nil {
			break
		}
	}

	return
}

// CloseWithReason closes the connection and logs the reason
func (s *Shard) CloseWithReason(reason error) error {
	s.log(LogLevelWarn, "%s: closing connection", reason)
	return s.Close()
}

// Close closes the current session
func (s *Shard) Close() (err error) {
	if err = s.conn.Close(); err != nil {
		return
	}

	s.log(LogLevelInfo, "Cleanly closed connection")
	return
}

func (s *Shard) readPacket(fn func(*types.ReceivePacket) error) (err error) {
	d, err := s.conn.Read()
	if err != nil {
		return
	}

	p := s.packets.Get().(*types.ReceivePacket)
	defer s.packets.Put(p)

	err = json.Unmarshal(d, p)
	if err != nil {
		return
	}
	s.log(LogLevelDebug, "received packet (%d): %s", p.Op, p.Event)

	if fn != nil {
		err = fn(p)
		if err != nil {
			return
		}
	}

	if s.opts.OnPacket != nil {
		s.opts.OnPacket(p)
	}

	if s.opts.Output != nil {
		s.opts.Output.Write(d)
	}

	err = s.handlePacket(p)
	return
}

// expectPacket reads the next packet, verifies its operation code, and event name (if applicable)
func (s *Shard) expectPacket(op types.GatewayOp, event types.GatewayEvent, handler func(*types.ReceivePacket) error) (err error) {
	err = s.readPacket(func(pk *types.ReceivePacket) error {
		if pk.Op != op {
			return fmt.Errorf("expected op to be %d, got %d", op, pk.Op)
		}

		if op == types.GatewayOpDispatch && pk.Event != event {
			return fmt.Errorf("expected event to be %s, got %s", event, pk.Event)
		}

		if handler != nil {
			return handler(pk)
		}

		return nil
	})

	return
}

// handlePacket handles a packet according to its operation code
func (s *Shard) handlePacket(p *types.ReceivePacket) (err error) {
	switch p.Op {
	case types.GatewayOpDispatch:
		return s.handleDispatch(p)

	case types.GatewayOpHeartbeat:
		return s.sendHeartbeat()

	case types.GatewayOpReconnect:
		if err = s.CloseWithReason(ErrReconnectReceived); err != nil {
			return
		}

	case types.GatewayOpInvalidSession:
		resumable := new(bool)
		if err = json.Unmarshal(p.Data, resumable); err != nil {
			return
		}

		if *resumable {
			if err = s.sendResume(); err != nil {
				return
			}

			s.log(LogLevelDebug, "Sent resume in response to invalid resumable session")
			return
		}

		time.Sleep(time.Second * time.Duration(rand.Intn(5)+1))
		if err = s.sendIdentify(); err != nil {
			return
		}

		s.log(LogLevelDebug, "Sent identify in response to invalid non-resumable session")

	case types.GatewayOpHeartbeatACK:
		s.acks <- struct{}{}
	}

	return
}

// handleDispatch handles dispatch packets
func (s *Shard) handleDispatch(p *types.ReceivePacket) (err error) {
	switch p.Event {
	case types.GatewayEventReady:
		r := new(types.Ready)
		if err = json.Unmarshal(p.Data, r); err != nil {
			return
		}

		s.sessionID = r.SessionID

		s.log(LogLevelDebug, "Using version: %d", r.Version)
		s.logTrace(r.Trace)

	case types.GatewayEventResumed:
		r := new(types.Resumed)
		if err = json.Unmarshal(p.Data, r); err != nil {
			return
		}

		s.logTrace(r.Trace)
	}

	return
}

func (s *Shard) handleHello(stop chan struct{}) func(*types.ReceivePacket) error {
	return func(p *types.ReceivePacket) (err error) {
		h := new(types.Hello)
		if err = json.Unmarshal(p.Data, h); err != nil {
			return
		}

		s.logTrace(h.Trace)
		go s.startHeartbeater(time.Duration(h.HeartbeatInterval)*time.Millisecond, stop)
		return
	}
}

// handleClose handles the WebSocket close event. Returns whether the session is recoverable.
func (s *Shard) handleClose(err error) (recoverable bool) {
	recoverable = websocket.IsUnexpectedCloseError(err, types.CloseAuthenticationFailed, types.CloseInvalidShard, types.CloseShardingRequired)
	if recoverable {
		s.log(LogLevelError, "received recoverable close code (%s): reconnecting", err)
	} else {
		s.log(LogLevelError, "received unrecovereable close code (%s)", err)
	}
	return
}

// SendPacket sends a packet
func (s *Shard) SendPacket(op types.GatewayOp, data interface{}) error {
	s.log(LogLevelDebug, "sending packet (%d): %+v", op, data)
	d, err := json.Marshal(&types.SendPacket{
		Op:   op,
		Data: data,
	})
	if err != nil {
		return err
	}

	s.limiter.lock()
	s.connMu.Lock()
	defer s.connMu.Unlock()

	_, err = s.conn.Write(d)
	return err
}

// sendIdentify sends an identify packet
func (s *Shard) sendIdentify() error {
	// TODO: rate limit identify packets
	return s.SendPacket(types.GatewayOpIdentify, s.opts.Identify)
}

// sendResume sends a resume packet
func (s *Shard) sendResume() error {
	return s.SendPacket(types.GatewayOpResume, &types.Resume{
		Token:     s.opts.Identify.Token,
		SessionID: s.sessionID,
		Seq:       types.Seq(atomic.LoadUint64(s.seq)),
	})
}

// sendHeartbeat sends a heartbeat packet
func (s *Shard) sendHeartbeat() error {
	return s.SendPacket(types.GatewayOpHeartbeat, atomic.LoadUint64(s.seq))
}

// startHeartbeater calls sendHeartbeat on the provided interval
func (s *Shard) startHeartbeater(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	acked := true

	s.log(LogLevelInfo, "starting heartbeat at interval %d/s", interval/time.Second)
	for {
		select {
		case <-s.acks:
			acked = true
		case <-t.C:
			if !acked {
				s.CloseWithReason(ErrHeartbeatUnacknowledged)
				return
			}

			if err := s.sendHeartbeat(); err != nil {
				s.log(LogLevelError, "error sending automatic heartbeat: %s", err)
				return
			}
			acked = false

		case <-stop:
			return
		}
	}
}

// gatewayURL returns the Gateway URL with appropriate query parameters
func (s *Shard) gatewayURL() string {
	query := url.Values{
		"v":        {s.opts.Version},
		"encoding": {"json"},
		"compress": {"zstd-stream"},
	}

	return s.Gateway.URL + "/?" + query.Encode()
}
