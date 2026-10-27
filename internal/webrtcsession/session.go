// Package webrtcsession owns the signaling WebSocket, the PeerConnection, and
// the persistent rpc/input DataChannels. Explicit state machine + generation;
// single teardown path; no auto-retry (the Agent decides whether to call
// again).
package webrtcsession

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/logger"
	"arkkvm-mcp/internal/signaling"
	"arkkvm-mcp/internal/toolerr"
)

// State is the connection state reported by the session state machine.
type State string

const (
	StateIdle       State = "idle"
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateClosing    State = "closing"
	StateFailed     State = "failed"
)

// Status is the connection snapshot surfaced by device_status.
type Status struct {
	State      State  `json:"state"`
	Generation int64  `json:"generation"`
	PCState    string `json:"pcState,omitempty"`
	RPCReady   bool   `json:"rpcReady"`
	InputReady bool   `json:"inputReady"`
}

// DeadEvent is delivered to onDead listeners on every teardown.
type DeadEvent struct {
	Reason      string
	Code        toolerr.ToolErrorCode
	Generation  int64
	Intentional bool
}

// Credentials carries device-mode cookie+CSRF or the cloud token.
type Credentials struct {
	Cookie    string
	CSRFToken string
	Token     string
}

// Deps wire the session to its environment.
type Deps struct {
	BaseURL             string
	IceServers          []config.IceServer
	ClientType          string
	ConnectTimeoutMs    int64
	DisconnectedGraceMs int64
	// GetCredentials: device mode returns agent cookie + CSRF; cloud mode the
	// cloud session token.
	GetCredentials func() Credentials
	// TargetDeviceID: cloud mode, sent as the signaling `id` query param.
	TargetDeviceID func() string
	// ResolveIceServers optionally resolves per-connection ICE (cloud TURN
	// config). A non-empty result takes precedence over IceServers.
	ResolveIceServers func() ([]config.IceServer, error)
	// RefreshCredentials is a one-shot credential refresh on signaling 401/403.
	RefreshCredentials func() (bool, error)
	// OnVideoTrack receives the remote HDMI video track.
	OnVideoTrack func(track *webrtc.TrackRemote)
	// OnRPCNotification receives device → client JSON-RPC notifications on
	// the rpc DataChannel (no id): controlConflict / controlChanged / …
	OnRPCNotification func(method string, params any)
}

const (
	// DefaultHandshakeTimeoutMs covers signaling open + datachannel wait. 60s:
	// the first cloud-relayed connect routinely spends tens of seconds in TURN
	// allocation before ICE completes.
	DefaultHandshakeTimeoutMs = 60_000
	// DefaultRPCTimeoutMs is the RPC response budget once the channels are up.
	DefaultRPCTimeoutMs = 30_000
	// DefaultDisconnectedGraceMs: `disconnected` stays
	// live until this grace elapses.
	DefaultDisconnectedGraceMs = 2_500
	mcpClientType              = "agent"
)

func toWebSocketURL(baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/webrtc/signaling/client"
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	default:
		base.Scheme = "ws"
	}
	return base.String(), nil
}

func encodeSdp(v any) string {
	data, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(data)
}

type signalingMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	// Cloud relay stamps.
	ClientID  string `json:"client_id"`
	SessionID string `json:"session_id"`
}

type pendingRPC struct {
	ch     chan rpcOutcome
	method string
}

type rpcOutcome struct {
	result any
	err    error
}

// Session owns the signaling WebSocket, PeerConnection, and DataChannels for
// one device connection.
type Session struct {
	deps Deps

	mu              sync.Mutex
	state           State
	generation      int64
	ws              *websocket.Conn
	pc              *webrtc.PeerConnection
	rpcChannel      *webrtc.DataChannel
	inputChannel    *webrtc.DataChannel
	connectDone     chan struct{}
	connectErr      error
	nextRequestID   int64
	pending         map[int64]chan rpcOutcome
	deadListeners   map[int]func(DeadEvent)
	nextListenerID  int
	disconnectedGen int64 // generation a disconnected-grace timer is watching, 0 = none
	cloudClientID   string
	webrtcSessionID string
	outboundIce     *signaling.OutboundIceGate

	// signaling phase dispatch (handshake vs connected)
	phaseMu sync.Mutex
	phase   phaseHandler

	writeMu sync.Mutex
}

// New builds a session.
func New(deps Deps) *Session {
	if !strings.HasSuffix(deps.BaseURL, "/") {
		deps.BaseURL += "/"
	}
	if deps.ClientType == "" {
		deps.ClientType = mcpClientType
	}
	if deps.ConnectTimeoutMs == 0 {
		deps.ConnectTimeoutMs = DefaultHandshakeTimeoutMs
	}
	if deps.DisconnectedGraceMs == 0 {
		deps.DisconnectedGraceMs = DefaultDisconnectedGraceMs
	}
	return &Session{
		deps:          deps,
		state:         StateIdle,
		nextRequestID: 1,
		pending:       make(map[int64]chan rpcOutcome),
		deadListeners: make(map[int]func(DeadEvent)),
	}
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{State: s.state, Generation: s.generation}
	if s.pc != nil {
		st.PCState = s.pc.ConnectionState().String()
	}
	st.RPCReady = channelOpen(s.rpcChannel)
	st.InputReady = channelOpen(s.inputChannel)
	return st
}

func (s *Session) Generation() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// OnDead registers a teardown listener; the returned func removes it.
func (s *Session) OnDead(listener func(DeadEvent)) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextListenerID
	s.nextListenerID++
	s.deadListeners[id] = listener
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.deadListeners, id)
	}
}

func channelOpen(dc *webrtc.DataChannel) bool {
	return dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen
}

func (s *Session) liveLocked() bool {
	if s.state != StateConnected {
		return false
	}
	if s.pc == nil {
		return false
	}
	switch s.pc.ConnectionState() {
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		return false
	}
	// `disconnected` stays live until the grace timer calls fail().
	return channelOpen(s.rpcChannel) && channelOpen(s.inputChannel)
}

// EnsureConnected is the lazy connect. No-op when live. Concurrent callers
// share one handshake. Does not retry after failure within the same call.
func (s *Session) EnsureConnected(ctx context.Context) error {
	s.mu.Lock()
	if s.liveLocked() {
		s.mu.Unlock()
		return nil
	}
	if s.state == StateConnecting && s.connectDone != nil {
		done := s.connectDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.connectErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.state == StateClosing || s.state == StateFailed {
		gen := s.generation
		state := s.state
		s.mu.Unlock()
		return toolerr.NewConnectionError(toolerr.CodeConnectionNotReady,
			fmt.Sprintf("WebRTC session is %s; wait for idle then ensureConnected again", state), gen)
	}
	s.state = StateConnecting
	s.generation++
	gen := s.generation
	done := make(chan struct{})
	s.connectDone = done
	s.mu.Unlock()

	logger.Info("WebRTC session connecting", map[string]any{"baseUrl": s.deps.BaseURL, "generation": gen})

	err := s.handshake(ctx, gen, 0)
	s.mu.Lock()
	if err == nil {
		if gen != s.generation {
			err = toolerr.NewConnectionError(toolerr.CodeConnectionLost, "WebRTC generation changed during connect", gen)
		} else if !s.controlPlaneOpenLocked() {
			err = toolerr.NewConnectionError(toolerr.CodeConnectionFailed,
				"WebRTC data channels not open after handshake", gen)
		} else {
			s.state = StateConnected
			s.attachLifetimeObservers(gen)
			logger.Info("WebRTC session connected", map[string]any{"generation": gen})
		}
	}
	if err != nil && gen == s.generation && (s.state == StateConnecting || s.state == StateConnected) {
		// Deferred teardown for the failed handshake of this generation.
		s.mu.Unlock()
		s.teardown(gen, err.Error(), toolerr.CodeConnectionFailed, false)
		s.mu.Lock()
	}
	if gen == s.generation {
		s.connectErr = err
		s.connectDone = nil
		close(done)
	}
	s.mu.Unlock()

	if err != nil {
		// ConnectionError / DeviceAuthError propagate as-is;
		// everything else becomes CONNECTION_FAILED.
		var connErr *toolerr.ConnectionError
		var authErr *toolerr.DeviceAuthError
		if asConnectionError(err, &connErr) || asDeviceAuthError(err, &authErr) {
			return err
		}
		return toolerr.NewConnectionError(toolerr.CodeConnectionFailed, err.Error(), gen)
	}
	return nil
}

// GetPeerConnection returns the live peer connection for extra channels
// (uploads, forwarding bridges).
func (s *Session) GetPeerConnection() (*webrtc.PeerConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.liveLocked() || s.pc == nil {
		code := toolerr.CodeConnectionNotReady
		if s.state != StateIdle {
			code = toolerr.CodeConnectionLost
		}
		return nil, toolerr.NewConnectionError(code, "WebRTC peer connection is unavailable", s.generation)
	}
	return s.pc, nil
}

// RPC performs one JSON-RPC request on the rpc DataChannel and awaits the
// response. A timeout tears the stale session down; the Agent decides whether
// to call again.
func (s *Session) RPC(ctx context.Context, method string, params any, timeoutMs int64) (any, error) {
	if err := s.EnsureConnected(ctx); err != nil {
		return nil, err
	}
	if timeoutMs <= 0 {
		timeoutMs = DefaultRPCTimeoutMs
	}
	s.mu.Lock()
	if !s.liveLocked() || !channelOpen(s.rpcChannel) {
		gen := s.generation
		s.mu.Unlock()
		return nil, toolerr.NewConnectionError(toolerr.CodeConnectionNotReady, "RPC data channel is not open", gen)
	}
	gen := s.generation
	id := s.nextRequestID
	s.nextRequestID++
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	dc := s.rpcChannel
	ch := make(chan rpcOutcome, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	timer := time.AfterFunc(time.Duration(timeoutMs)*time.Millisecond, func() {
		s.mu.Lock()
		if _, ok := s.pending[id]; !ok {
			s.mu.Unlock()
			return
		}
		delete(s.pending, id)
		s.mu.Unlock()
		logger.Warn("RPC request timed out", map[string]any{"method": method, "id": id, "timeoutMs": timeoutMs})
		msg := fmt.Sprintf("RPC request timed out: %s (#%d)", method, id)
		// Tear down stale session; Agent decides whether to call again.
		s.Fail(gen, msg)
		ch <- rpcOutcome{err: toolerr.NewConnectionError(toolerr.CodeConnectionLost, msg, gen)}
	})

	sendErr := func() error {
		s.mu.Lock()
		current := gen == s.generation && channelOpen(s.rpcChannel)
		s.mu.Unlock()
		if !current {
			return toolerr.NewConnectionError(toolerr.CodeConnectionLost, "RPC data channel closed before send", gen)
		}
		return dc.Send(payload)
	}()
	if sendErr != nil {
		timer.Stop()
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		msg := sendErr.Error()
		var connErr *toolerr.ConnectionError
		if !asConnectionError(sendErr, &connErr) {
			connErr = toolerr.NewConnectionError(toolerr.CodeConnectionLost, msg, gen)
		}
		s.Fail(gen, connErr.Error())
		ch <- rpcOutcome{err: connErr}
	}
	logger.Debug("RPC request sent", map[string]any{"method": method, "id": id})

	select {
	case out := <-ch:
		timer.Stop()
		return out.result, out.err
	case <-ctx.Done():
		timer.Stop()
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// SendInput writes one input report on the input DataChannel (fire-and-forget).
func (s *Session) SendInput(ctx context.Context, method string, params any) error {
	if err := s.EnsureConnected(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	if !s.liveLocked() || !channelOpen(s.inputChannel) {
		gen := s.generation
		s.mu.Unlock()
		return toolerr.NewConnectionError(toolerr.CodeConnectionNotReady, "Input data channel is not open", gen)
	}
	dc := s.inputChannel
	id := s.nextRequestID
	s.nextRequestID++
	s.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	return dc.Send(payload)
}

// Close is the intentional shutdown.
func (s *Session) Close() {
	gen := s.Generation()
	s.mu.Lock()
	if s.state == StateIdle {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.teardown(gen, "Session closed", toolerr.CodeConnectionLost, true)
}

// Invalidate marks the session dead (e.g. silent media); the Agent may
// EnsureConnected on the next tool call.
func (s *Session) Invalidate(reason string) {
	s.Fail(s.Generation(), reason)
}

// Fail tears the session down for a non-intentional reason.
func (s *Session) Fail(gen int64, reason string) {
	s.mu.Lock()
	if gen != s.generation || s.state == StateIdle || s.state == StateClosing || s.state == StateFailed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.teardown(gen, reason, toolerr.CodeConnectionLost, false)
}

func (s *Session) controlPlaneOpenLocked() bool {
	if s.pc == nil {
		return false
	}
	switch s.pc.ConnectionState() {
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		return false
	}
	return channelOpen(s.rpcChannel) && channelOpen(s.inputChannel)
}

func (s *Session) attachLifetimeObservers(gen int64) {
	// The ws read pump was started by handshake; swap its dispatch into the
	// post-connect mode (only close handling matters).
	s.setPhase(&connectedPhase{session: s, gen: gen})

	pc := s.pc
	if pc == nil {
		return
	}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.mu.Lock()
		if gen != s.generation {
			s.mu.Unlock()
			return
		}
		logger.Debug("WebRTC connection state changed", map[string]any{"state": state.String(), "generation": gen})
		if s.state == StateClosing || s.state == StateFailed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.Fail(gen, fmt.Sprintf("WebRTC connection %s", state.String()))
		case webrtc.PeerConnectionStateDisconnected:
			s.armDisconnectedGrace(gen)
		case webrtc.PeerConnectionStateConnected, webrtc.PeerConnectionStateConnecting:
			s.clearDisconnectedGrace(gen)
		}
	})

	if rpc := s.rpcChannel; rpc != nil {
		rpc.OnClose(func() { s.channelDead(gen, "rpc") })
	}
	if input := s.inputChannel; input != nil {
		input.OnClose(func() { s.channelDead(gen, "input") })
	}
}

func (s *Session) channelDead(gen int64, label string) {
	s.mu.Lock()
	if gen != s.generation || s.state == StateClosing || s.state == StateFailed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.Fail(gen, fmt.Sprintf("%s data channel closed", label))
}

func (s *Session) armDisconnectedGrace(gen int64) {
	grace := s.deps.DisconnectedGraceMs
	s.mu.Lock()
	s.disconnectedGen = gen
	s.mu.Unlock()
	time.AfterFunc(time.Duration(grace)*time.Millisecond, func() {
		s.mu.Lock()
		if gen != s.generation || s.disconnectedGen != gen {
			s.mu.Unlock()
			return
		}
		pcState := webrtc.PeerConnectionStateClosed
		if s.pc != nil {
			pcState = s.pc.ConnectionState()
		}
		s.mu.Unlock()
		switch pcState {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.Fail(gen, "WebRTC connection disconnected")
		}
	})
}

func (s *Session) clearDisconnectedGrace(gen int64) {
	s.mu.Lock()
	if s.disconnectedGen == gen {
		s.disconnectedGen = 0
	}
	s.mu.Unlock()
}

func (s *Session) teardown(gen int64, reason string, code toolerr.ToolErrorCode, intentional bool) {
	s.mu.Lock()
	if s.state == StateIdle {
		s.mu.Unlock()
		return
	}
	if intentional {
		s.state = StateClosing
	} else {
		s.state = StateFailed
	}
	s.disconnectedGen = 0
	s.connectDone = nil

	if intentional {
		logger.Info("WebRTC session closing", map[string]any{"generation": gen, "reason": reason})
	} else {
		logger.Warn("WebRTC session failed", map[string]any{"generation": gen, "reason": reason})
	}

	for id, ch := range s.pending {
		ch <- rpcOutcome{err: toolerr.NewConnectionError(code, reason, gen)}
		delete(s.pending, id)
	}

	rpc := s.rpcChannel
	input := s.inputChannel
	s.rpcChannel = nil
	s.inputChannel = nil
	pc := s.pc
	s.pc = nil
	ws := s.ws
	s.ws = nil
	listeners := make([]func(DeadEvent), 0, len(s.deadListeners))
	for _, l := range s.deadListeners {
		listeners = append(listeners, l)
	}
	s.mu.Unlock()

	if rpc != nil {
		rpc.OnMessage(nil)
		rpc.OnClose(nil)
		_ = rpc.Close()
	}
	if input != nil {
		input.OnMessage(nil)
		input.OnClose(nil)
		_ = input.Close()
	}
	if pc != nil {
		pc.OnConnectionStateChange(nil)
		pc.OnICECandidate(nil)
		pc.OnTrack(nil)
		_ = pc.Close()
	}
	if ws != nil {
		_ = ws.Close()
	}

	dead := DeadEvent{Reason: reason, Code: code, Generation: gen, Intentional: intentional}
	for _, l := range listeners {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("WebRTC onDead listener failed", fmt.Errorf("%v", r), nil)
				}
			}()
			l(dead)
		}()
	}

	s.mu.Lock()
	s.state = StateIdle
	s.cloudClientID = ""
	s.webrtcSessionID = ""
	if s.outboundIce != nil {
		s.outboundIce.Clear()
		s.outboundIce = nil
	}
	s.mu.Unlock()
	s.setPhase(nil)
	logger.Info("WebRTC session idle", map[string]any{"generation": gen})
}

// handshake performs signaling dial + offer/answer + channel wait.
func (s *Session) handshake(ctx context.Context, gen int64, attempt int) error {
	justLoggedIn := false
	initialCreds := s.deps.GetCredentials()
	if s.deps.RefreshCredentials != nil && initialCreds.Cookie == "" && initialCreds.Token == "" {
		justLoggedIn, _ = s.deps.RefreshCredentials()
	}

	creds := s.deps.GetCredentials()
	var deviceID string
	if s.deps.TargetDeviceID != nil {
		deviceID = s.deps.TargetDeviceID()
	}
	signalURL, err := toWebSocketURL(s.deps.BaseURL)
	if err != nil {
		return err
	}
	u, err := url.Parse(signalURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("clientType", s.deps.ClientType)
	if creds.CSRFToken != "" {
		q.Set("csrf-token", creds.CSRFToken)
	}
	if deviceID != "" {
		q.Set("id", deviceID)
	}
	if creds.Token != "" {
		q.Set("token", creds.Token)
	}
	u.RawQuery = q.Encode()
	signalURL = u.String()

	header := http.Header{}
	if creds.Cookie != "" {
		header.Set("Cookie", creds.Cookie)
	}
	if creds.Token != "" {
		header.Set("Authorization", "Bearer "+creds.Token)
	}

	// Cloud relays hand out TURN config per connection; a failed lookup only
	// downgrades to the statically configured ICE servers, which may still
	// connect in friendly network conditions.
	iceServers := s.deps.IceServers
	if s.deps.ResolveIceServers != nil {
		dynamic, err := s.deps.ResolveIceServers()
		if err != nil {
			logger.Warn("Dynamic ICE server resolution failed; using configured ICE servers",
				map[string]any{"error": err.Error(), "generation": gen})
		} else if len(dynamic) > 0 {
			iceServers = dynamic
		}
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, time.Duration(s.deps.ConnectTimeoutMs)*time.Millisecond)
	defer cancelDial()
	handshakeTimeout := time.Duration(s.deps.ConnectTimeoutMs) * time.Millisecond
	dialer := websocket.Dialer{HandshakeTimeout: handshakeTimeout}
	wsConn, resp, dialErr := dialer.DialContext(dialCtx, signalURL, header)
	if dialErr != nil {
		s.mu.Lock()
		current := gen == s.generation
		s.mu.Unlock()
		if !current {
			return toolerr.NewConnectionError(toolerr.CodeConnectionLost, "WebRTC generation changed", gen)
		}
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			status := resp.StatusCode
			logger.Error("WebRTC signaling handshake rejected", nil, map[string]any{"httpStatus": status})
			if creds.Token != "" {
				return toolerr.NewDeviceAuthError(status,
					fmt.Sprintf("Cloud session token rejected (HTTP %d): it may have expired or been revoked. Re-authenticate with kvm_cloud_login action=login.", status))
			}
			return toolerr.NewDeviceAuthError(status, "")
		}
		if resp != nil {
			return fmt.Errorf("WebRTC signaling handshake failed: HTTP %d", resp.StatusCode)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if dialCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("WebRTC signaling open timed out")
		}
		return dialErr
	}

	s.mu.Lock()
	if gen != s.generation {
		s.mu.Unlock()
		_ = wsConn.Close()
		return toolerr.NewConnectionError(toolerr.CodeConnectionLost, "WebRTC generation changed", gen)
	}
	s.ws = wsConn
	s.mu.Unlock()

	logger.Debug("Opening WebRTC signaling socket", map[string]any{
		"path":              u.Path,
		"clientType":        s.deps.ClientType,
		"authenticated":     creds.Cookie != "" || creds.Token != "",
		"cookieAuth":        creds.Cookie != "",
		"tokenAuth":         creds.Token != "",
		"targetDeviceId":    deviceID,
		"csrfTokenProvided": creds.CSRFToken != "",
		"generation":        gen,
	})

	// Read pump: dispatches signaling frames until the session is torn down.
	phase := newHandshakePhase(ctx, gen, s.deps.ConnectTimeoutMs, s, iceServers)
	s.setPhase(phase)
	go s.readPump()

	if err := phase.run(); err != nil {
		// One-shot credential refresh on signaling 401/403.
		var authErr *toolerr.DeviceAuthError
		if asDeviceAuthError(err, &authErr) && s.deps.RefreshCredentials != nil && !justLoggedIn && attempt == 0 {
			s.setPhase(nil)
			_ = wsConn.Close()
			_, _ = s.deps.RefreshCredentials()
			return s.handshake(ctx, gen, attempt+1)
		}
		return err
	}
	return nil
}
