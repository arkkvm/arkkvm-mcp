package webrtcsession

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/logger"
	"arkkvm-mcp/internal/protocol"
	"arkkvm-mcp/internal/signaling"
	"arkkvm-mcp/internal/toolerr"
)

// sdpPayload is the JSON shape inside the base64 signaling data field.
type sdpPayload struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// decodeAnswerPayload parses the answer: the device sends the answer
// `data` field as base64(JSON({type,sdp})). Plain JSON objects are accepted
// for robustness against device variants.
func decodeAnswerPayload(data json.RawMessage) (sdpPayload, error) {
	var encoded string
	if err := json.Unmarshal(data, &encoded); err == nil {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return sdpPayload{}, fmt.Errorf("answer data base64 decode: %w", err)
		}
		data = raw
	}
	var sd sdpPayload
	if err := json.Unmarshal(data, &sd); err != nil {
		return sdpPayload{}, err
	}
	return sd, nil
}

// phaseHandler receives signaling frames from the ws read pump. The handshake
// phase drives offer/answer; the connected phase only reacts to close.
type phaseHandler interface {
	onMessage(data []byte)
	onClose()
}

func (s *Session) setPhase(p phaseHandler) {
	s.phaseMu.Lock()
	s.phase = p
	s.phaseMu.Unlock()
}

func (s *Session) dispatchWS(data []byte) {
	s.phaseMu.Lock()
	p := s.phase
	s.phaseMu.Unlock()
	if p != nil {
		p.onMessage(data)
	}
}

func (s *Session) dispatchWSClose() {
	s.phaseMu.Lock()
	p := s.phase
	s.phaseMu.Unlock()
	if p != nil {
		p.onClose()
	}
}

// handshakePhase drives the signaling handshake.
type handshakePhase struct {
	session    *Session
	gen        int64
	timeout    time.Duration
	ctx        context.Context
	iceServers []config.IceServer

	mu                   sync.Mutex
	started              bool
	settled              bool
	remoteDescriptionSet bool
	queuedCandidates     []json.RawMessage
	done                 chan error

	candidateCh chan json.RawMessage
	pc          *webrtc.PeerConnection
}

func newHandshakePhase(ctx context.Context, gen int64, timeoutMs int64, s *Session, iceServers []config.IceServer) *handshakePhase {
	return &handshakePhase{
		session:    s,
		gen:        gen,
		timeout:    time.Duration(timeoutMs) * time.Millisecond,
		ctx:        ctx,
		iceServers: iceServers,
		done:       make(chan error, 1),
	}
}

func asConnectionError(err error, target **toolerr.ConnectionError) bool {
	return errors.As(err, target)
}

func asDeviceAuthError(err error, target **toolerr.DeviceAuthError) bool {
	return errors.As(err, target)
}

func (p *handshakePhase) fail(err error) {
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return
	}
	p.settled = true
	p.mu.Unlock()
	logger.Error("WebRTC connection failed", err, nil)
	p.done <- err
}

func (p *handshakePhase) succeed() {
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return
	}
	p.settled = true
	p.mu.Unlock()
	p.done <- nil
}

// run blocks until the handshake settles (answer processed + channels open),
// the socket closes, or the caller context is cancelled.
func (p *handshakePhase) run() error {
	select {
	case err := <-p.done:
		return err
	case <-p.ctx.Done():
		p.fail(p.ctx.Err())
		return p.ctx.Err()
	}
}

func (p *handshakePhase) onMessage(data []byte) {
	var message signalingMessage
	if err := json.Unmarshal(data, &message); err != nil {
		p.fail(err)
		return
	}
	p.mu.Lock()
	gen := p.gen
	started := p.started
	p.mu.Unlock()

	if message.Type == "device-metadata" && !started {
		p.mu.Lock()
		p.started = true
		p.mu.Unlock()
		if minted := message.ClientID; minted != "" {
			p.session.mu.Lock()
			p.session.cloudClientID = minted
			p.session.mu.Unlock()
		}
		// Fire-and-forget start; failures settle the handshake.
		go func() {
			if err := p.startPeerConnection(); err != nil {
				p.fail(err)
			}
		}()
		return
	}
	if !started {
		return
	}

	if message.Type == "error" {
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(message.Data, &payload)
		code := payload.Code
		if code == "" {
			code = "signalingError"
		}
		detail := payload.Message
		if detail == "" {
			detail = "WebRTC signaling request rejected"
		}
		p.fail(fmt.Errorf("%s: %s", code, detail))
		p.session.closeSignaling()
		return
	}

	p.session.mu.Lock()
	pc := p.session.pc
	p.session.mu.Unlock()
	if pc == nil {
		return
	}

	switch message.Type {
	case "answer":
		var sd sdpPayload
		if len(message.Data) > 0 {
			decoded, err := decodeAnswerPayload(message.Data)
			if err != nil {
				p.fail(err)
				return
			}
			sd = decoded
		}
		if message.SessionID != "" {
			p.session.mu.Lock()
			p.session.webrtcSessionID = message.SessionID
			clientID := p.session.cloudClientID
			sessionID := p.session.webrtcSessionID
			gate := p.session.outboundIce
			p.session.mu.Unlock()
			held := gate.Flush(sessionID)
			p.session.writeOutboundIce(held, clientID, sessionID)
		}
		logger.Debug("WebRTC answer received", nil)
		// Run off the read pump so ICE candidates keep flowing during the
		// datachannel wait.
		go func() {
			if gen != p.session.Generation() {
				return
			}
			if err := p.handleAnswer(pc, sd); err != nil {
				p.fail(err)
			}
		}()
	case "new-ice-candidate":
		if len(message.Data) == 0 || string(message.Data) == "null" {
			return
		}
		p.mu.Lock()
		if p.remoteDescriptionSet {
			p.mu.Unlock()
			select {
			case p.candidateCh <- message.Data:
			case <-time.After(10 * time.Second):
				// Applier stuck; treat like a failed candidate.
				p.fail(fmt.Errorf("ICE candidate apply timed out"))
			}
			return
		}
		p.queuedCandidates = append(p.queuedCandidates, message.Data)
		p.mu.Unlock()
	}
	_ = gen
}

func (p *handshakePhase) onClose() {
	p.mu.Lock()
	settled := p.settled
	p.mu.Unlock()
	if settled {
		return
	}
	logger.Warn("Device signaling socket closed during handshake", nil)
	p.fail(fmt.Errorf("Device signaling connection closed"))
}

// handleAnswer sets the remote description, flushes queued candidates, waits
// for both control channels, and settles.
func (p *handshakePhase) handleAnswer(pc *webrtc.PeerConnection, sd sdpPayload) error {
	if p.session.Generation() != p.gen {
		return nil
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sd.SDP,
	}); err != nil {
		return err
	}
	p.mu.Lock()
	p.remoteDescriptionSet = true
	queued := p.queuedCandidates
	p.queuedCandidates = nil
	applier := p.candidateCh
	p.mu.Unlock()

	for _, candidate := range queued {
		applier <- candidate
	}
	if err := waitForDataChannels(p.session, p.ctx, p.timeout); err != nil {
		return err
	}
	logger.Info("WebRTC connection established", nil)
	p.succeed()
	return nil
}

// startPeerConnection builds the pion peer connection with the H264-only
// video codec set (OPUS+PCMU audio), adds the four transceivers
// and the rpc/input DataChannels, then sends the offer.
func (p *handshakePhase) startPeerConnection() error {
	if p.session.Generation() != p.gen {
		return nil
	}
	mediaEngine := buildMediaEngine()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: toPionICEServers(p.iceServers),
	})
	if err != nil {
		return err
	}
	gen := p.gen
	s := p.session

	s.mu.Lock()
	s.pc = pc
	s.mu.Unlock()

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.mu.Lock()
		current := gen == s.generation
		s.mu.Unlock()
		if !current || track.Kind() != webrtc.RTPCodecTypeVideo {
			return
		}
		if s.deps.OnVideoTrack != nil {
			s.deps.OnVideoTrack(track)
		}
		logger.Info("HDMI video track received", nil)
	})

	if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		return err
	}
	if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		return err
	}
	if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return err
	}
	if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		return err
	}

	ordered := true
	rpcChannel, err := pc.CreateDataChannel("rpc", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	inputChannel, err := pc.CreateDataChannel("input", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.rpcChannel = rpcChannel
	s.inputChannel = inputChannel
	s.mu.Unlock()
	rpcChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.dispatchRPCMessage(gen, msg.Data)
	})

	p.mu.Lock()
	p.pc = pc
	p.candidateCh = make(chan json.RawMessage, 32)
	p.mu.Unlock()
	go p.candidateApplier()

	// Cloud device WS fail-closes ICE without top-level session_id; hold until
	// answer. LAN routes by socket id and must not buffer.
	requireSessionID := s.deps.GetCredentials().Token != ""
	s.mu.Lock()
	s.webrtcSessionID = ""
	s.outboundIce = signaling.NewOutboundIceGate(requireSessionID)
	s.mu.Unlock()

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		s.mu.Lock()
		current := gen == s.generation
		clientID := s.cloudClientID
		sessionID := s.webrtcSessionID
		gate := s.outboundIce
		s.mu.Unlock()
		if !current || candidate == nil {
			return
		}
		ready := gate.Push(candidate.ToJSON(), sessionID)
		s.writeOutboundIce(ready, clientID, sessionID)
	})

	// Handshake-phase PC failure; post-connect handled by attachLifetimeObservers.
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.mu.Lock()
		settled := p.settled
		p.mu.Unlock()
		if gen != s.Generation() || settled {
			return
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			p.fail(fmt.Errorf("WebRTC connection %s", state.String()))
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	offerFrame := signaling.BuildCloudOfferFrame(
		encodeSdp(map[string]string{"type": "offer", "sdp": pc.LocalDescription().SDP}),
		s.deps.ClientType,
		s.cloudClientIDLocked(),
	)
	s.writeMu.Lock()
	err = s.writeJSON(offerFrame)
	s.writeMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// candidateApplier applies remote candidates strictly in order; an sdpMid
// mismatch falls back to sdpMLineIndex addressing.
func (p *handshakePhase) candidateApplier() {
	for candidate := range p.candidateCh {
		if p.session.Generation() != p.gen {
			return
		}
		p.mu.Lock()
		pc := p.pc
		p.mu.Unlock()
		if pc == nil {
			return
		}
		if err := applyCandidate(pc, candidate); err != nil {
			p.fail(err)
			return
		}
	}
}

func applyCandidate(pc *webrtc.PeerConnection, raw json.RawMessage) error {
	var value webrtc.ICECandidateInit
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if err := pc.AddICECandidate(value); err != nil {
		// Some peers fail with "Media section for sdpMid was not
		// found" unless sdpMid is dropped; replicate that fallback.
		if value.SDPMid != nil {
			value.SDPMid = nil
			if retryErr := pc.AddICECandidate(value); retryErr == nil {
				return nil
			}
		}
		return err
	}
	return nil
}

// connectedPhase is the post-connect ws behavior: only close matters.
type connectedPhase struct {
	session *Session
	gen     int64
}

func (p *connectedPhase) onMessage([]byte) {}

func (p *connectedPhase) onClose() {
	s := p.session
	s.mu.Lock()
	if p.gen != s.generation || s.state == StateClosing || s.state == StateFailed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	logger.Warn("Signaling socket closed after connect", map[string]any{"generation": p.gen})
	s.Fail(p.gen, "Device signaling connection closed")
}

// dispatchRPCMessage resolves pending RPC requests from rpc-channel frames
// and forwards device → client notifications (frames with no id).
func (s *Session) dispatchRPCMessage(gen int64, data []byte) {
	s.mu.Lock()
	current := gen == s.generation
	s.mu.Unlock()
	if !current {
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *float64        `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return
	}
	// Device → client notification (no id): controlConflict / controlChanged / …
	if message.ID == nil {
		if message.Method != "" && s.deps.OnRPCNotification != nil {
			var params any
			if len(message.Params) > 0 && string(message.Params) != "null" {
				_ = json.Unmarshal(message.Params, &params)
			}
			if params == nil {
				params = map[string]any{}
			}
			s.deps.OnRPCNotification(message.Method, params)
		}
		return
	}
	id := int64(*message.ID)
	s.mu.Lock()
	ch, ok := s.pending[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.pending, id)
	s.mu.Unlock()

	if message.Error != nil {
		rpcErr := protocol.RPCError{Code: message.Error.Code, Message: message.Error.Message}
		if len(message.Error.Data) > 0 && string(message.Error.Data) != "null" {
			var dataStr string
			if json.Unmarshal(message.Error.Data, &dataStr) == nil {
				rpcErr.Data = &dataStr
			} else {
				// Prefer data.code when the device returns a structured error
				// object (e.g. controlDenied); otherwise stringify the object.
				var dataObj struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(message.Error.Data, &dataObj) == nil && dataObj.Code != "" {
					dataStr = dataObj.Code
				} else {
					dataStr = string(message.Error.Data)
				}
				rpcErr.Data = &dataStr
			}
		}
		ch <- rpcOutcome{err: fmt.Errorf("%s", rpcErr.Error())}
		return
	}
	var result any
	if len(message.Result) > 0 {
		_ = json.Unmarshal(message.Result, &result)
	}
	ch <- rpcOutcome{result: result}
}

// waitForDataChannels polls until both control channels are open
// (a 25ms poll instead of channel callbacks).
func waitForDataChannels(s *Session, ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		rpcOpen := channelOpen(s.rpcChannel)
		inputOpen := channelOpen(s.inputChannel)
		s.mu.Unlock()
		if rpcOpen && inputOpen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("WebRTC data channels did not open")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *Session) cloudClientIDLocked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cloudClientID
}

// writeJSON writes one signaling frame; callers must hold writeMu.
func (s *Session) writeJSON(frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		return toolerr.NewConnectionError(toolerr.CodeConnectionLost, "signaling socket closed", 0)
	}
	return ws.WriteMessage(websocket.TextMessage, data)
}

// writeOutboundIce sends local ICE candidates that the outbound gate released.
func (s *Session) writeOutboundIce(candidates []any, clientID, sessionID string) {
	if len(candidates) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	for _, c := range candidates {
		if err := s.writeJSON(signaling.BuildCloudIceFrame(c, clientID, sessionID)); err != nil {
			return
		}
	}
}

// closeSignaling closes the signaling socket (gorilla Close is goroutine-safe).
func (s *Session) closeSignaling() {
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
}

// readPump reads signaling frames until the socket dies, dispatching to the
// active phase handler.
func (s *Session) readPump() {
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		return
	}
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			s.dispatchWSClose()
			return
		}
		s.dispatchWS(data)
	}
}

// buildMediaEngine registers the codec set: H264-only video,
// OPUS + PCMU audio, with the matching RTX codec.
func buildMediaEngine() *webrtc.MediaEngine {
	m := &webrtc.MediaEngine{}
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		PayloadType: 102,
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
	}, webrtc.RTPCodecTypeVideo)
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		PayloadType: 103,
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeRTX,
			ClockRate:   90000,
			SDPFmtpLine: "apt=102",
		},
	}, webrtc.RTPCodecTypeVideo)
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		PayloadType: 111,
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
	}, webrtc.RTPCodecTypeAudio)
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		PayloadType: 0,
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: 8000,
		},
	}, webrtc.RTPCodecTypeAudio)
	return m
}

func toPionICEServers(servers []config.IceServer) []webrtc.ICEServer {
	out := make([]webrtc.ICEServer, 0, len(servers))
	for _, server := range servers {
		entry := webrtc.ICEServer{Username: server.Username, Credential: server.Credential}
		switch urls := server.URLs.(type) {
		case string:
			entry.URLs = []string{urls}
		case []any:
			list := make([]string, 0, len(urls))
			for _, u := range urls {
				if s, ok := u.(string); ok {
					list = append(list, s)
				}
			}
			entry.URLs = list
		case []string:
			entry.URLs = append([]string(nil), urls...)
		}
		out = append(out, entry)
	}
	return out
}
