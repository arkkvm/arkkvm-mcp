// Package forwarding bridges local TCP/UDP ports to the controlled machine
// through dedicated ip_forward_<uuid> DataChannels. TCP opens one listener;
// every accepted connection gets its own startIpForward RPC + channel pair.
// UDP opens one channel and locks onto the first local peer.
package forwarding

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"arkkvm-mcp/internal/logger"
)

const (
	Prefix = "ip_forward_"
	// MaxSessions caps concurrent forwarding sessions.
	MaxSessions = 8
	// MaxTCPBridgesPerSession caps concurrent bridges in one TCP session.
	MaxTCPBridgesPerSession = 8
	// UDPMaxDatagram is the practical DataChannel message budget.
	UDPMaxDatagram = 1200
	// DCHighWatermark pauses TCP socket reads above this buffered amount.
	DCHighWatermark = 256 * 1024
	// DCLowWatermark resumes sending when buffered drops below this.
	DCLowWatermark = 64 * 1024
)

// Protocol is the forwarded transport.
type Protocol string

const (
	TCP Protocol = "tcp"
	UDP Protocol = "udp"
)

// Mode selects the device-side path resolution.
type Mode string

const (
	ModeUSB  Mode = "usb"
	ModeHost Mode = "host"
)

// OpenParams mirror the kvm_ip_forward_* tool arguments.
type OpenParams struct {
	Mode      Mode
	Host      string
	Port      int
	Protocol  Protocol
	LocalPort *int
}

// OpenResult is returned when the listener is live.
type OpenResult struct {
	SessionID    string `json:"sessionId"`
	LocalHost    string `json:"localHost"`
	LocalPort    int    `json:"localPort"`
	Protocol     string `json:"protocol"`
	ResolvedHost string `json:"resolvedHost,omitempty"`
	TargetPort   int    `json:"targetPort"`
	Mode         string `json:"mode"`
}

// SessionStatus is the in-memory status view.
type SessionStatus struct {
	SessionID     string `json:"sessionId"`
	Mode          string `json:"mode"`
	Protocol      string `json:"protocol"`
	LocalHost     string `json:"localHost"`
	LocalPort     int    `json:"localPort"`
	TargetPort    int    `json:"targetPort"`
	ResolvedHost  string `json:"resolvedHost,omitempty"`
	Host          string `json:"host,omitempty"`
	ActiveBridges int    `json:"activeBridges"`
	Closed        bool   `json:"closed"`
}

// RPCFn performs a device JSON-RPC call on the control plane.
type RPCFn func(ctx context.Context, method string, params any, timeoutMs int64) (any, error)

type bridge struct {
	channel *webrtc.DataChannel
	conn    net.Conn
	closed  bool
}

type Session struct {
	id           string
	mode         Mode
	protocol     Protocol
	targetHost   string
	targetPort   int
	resolvedHost string
	localHost    string
	localPort    int

	tcpListener net.Listener
	udpConn     *net.UDPConn
	udpChannel  *webrtc.DataChannel

	mu      sync.Mutex
	udpPeer *net.UDPAddr
	bridges map[*bridge]struct{}
	closed  bool
}

// Manager owns all forwarding sessions.
type Manager struct {
	// baseCtx outlives any single MCP request: forward listeners and their
	// per-connection RPCs must survive after the tool call that opened them
	// returns (request contexts are cancelled as soon as the response is sent).
	baseCtx  context.Context
	mu       sync.Mutex
	sessions map[string]*Session
	getPC    func() *webrtc.PeerConnection
	rpc      RPCFn
}

// NewManager wires the manager to the device client.
func NewManager(getPC func() *webrtc.PeerConnection, rpc RPCFn) *Manager {
	return &Manager{baseCtx: context.Background(), sessions: make(map[string]*Session), getPC: getPC, rpc: rpc}
}

// SessionCount reports the number of live sessions.
func (m *Manager) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func parseProtocol(value string) (Protocol, error) {
	p := strings.ToLower(strings.TrimSpace(value))
	switch p {
	case "tcp":
		return TCP, nil
	case "udp":
		return UDP, nil
	case "":
		return TCP, nil
	}
	return "", fmt.Errorf("protocol must be tcp or udp")
}

// Open starts a forwarding session listener.
func (m *Manager) Open(ctx context.Context, params OpenParams, timeoutMs int64) (*OpenResult, error) {
	m.mu.Lock()
	count := len(m.sessions)
	m.mu.Unlock()
	if count >= MaxSessions {
		return nil, fmt.Errorf("Too many IP forward sessions (max %d)", MaxSessions)
	}
	protocol, err := parseProtocol(string(params.Protocol))
	if err != nil {
		return nil, err
	}
	if params.Port < 1 || params.Port > 65535 {
		return nil, fmt.Errorf("port must be in 1..=65535")
	}
	if params.Mode == ModeHost {
		if strings.TrimSpace(params.Host) == "" {
			return nil, fmt.Errorf("kvm_ip_forward_host requires host")
		}
	} else if strings.TrimSpace(params.Host) != "" {
		return nil, fmt.Errorf("kvm_ip_forward_usb must not include host; peer IP is resolved from USB DHCP")
	}

	sessionID := newUUID()
	if protocol == TCP {
		return m.openTCP(ctx, sessionID, params, timeoutMs)
	}
	return m.openUDP(ctx, sessionID, params, timeoutMs)
}

// Status returns the in-memory session view.
func (m *Manager) Status(sessionID string) (*SessionStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[sessionID]
	if session == nil || session.closed {
		return nil, fmt.Errorf("Unknown or closed IP forward session: %s", sessionID)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return &SessionStatus{
		SessionID:     session.id,
		Mode:          string(session.mode),
		Protocol:      string(session.protocol),
		LocalHost:     session.localHost,
		LocalPort:     session.localPort,
		TargetPort:    session.targetPort,
		ResolvedHost:  session.resolvedHost,
		Host:          session.targetHost,
		ActiveBridges: len(session.bridges),
		Closed:        session.closed,
	}, nil
}

// Close tears one session down.
func (m *Manager) Close(sessionID string) error {
	m.mu.Lock()
	session := m.sessions[sessionID]
	m.mu.Unlock()
	if session == nil {
		return nil
	}
	return m.disposeSession(session)
}

// CloseAll disposes every session (connection death / client close).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		_ = m.disposeSession(s)
	}
}

func (m *Manager) require(sessionID string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[sessionID]
	if session == nil || session.closed {
		return nil, fmt.Errorf("Unknown or closed IP forward session: %s", sessionID)
	}
	return session, nil
}

func (m *Manager) startIpForwardRPC(ctx context.Context, mode Mode, host string, port int, protocol Protocol, timeoutMs int64) (string, string, error) {
	var params any
	if mode == ModeUSB {
		params = map[string]any{"target": "usb", "port": port, "protocol": string(protocol)}
	} else {
		params = map[string]any{"host": host, "port": port, "protocol": string(protocol)}
	}
	response, err := m.rpc(ctx, "startIpForward", params, timeoutMs)
	if err != nil {
		return "", "", err
	}
	record, _ := response.(map[string]any)
	dataChannel, _ := record["dataChannel"].(string)
	resolvedHost, _ := record["resolvedHost"].(string)
	if !strings.HasPrefix(dataChannel, Prefix) {
		return "", "", fmt.Errorf("Device returned an invalid ip forward data channel label")
	}
	return dataChannel, resolvedHost, nil
}

func (m *Manager) openTCP(ctx context.Context, sessionID string, params OpenParams, timeoutMs int64) (*OpenResult, error) {
	localPort := 0
	if params.LocalPort != nil {
		localPort = *params.LocalPort
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, err
	}
	addr := listener.Addr().(*net.TCPAddr)

	session := &Session{
		id:          sessionID,
		mode:        params.Mode,
		protocol:    TCP,
		targetHost:  strings.TrimSpace(params.Host),
		targetPort:  params.Port,
		localHost:   "127.0.0.1",
		localPort:   addr.Port,
		tcpListener: listener,
		bridges:     make(map[*bridge]struct{}),
	}
	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	go m.acceptLoop(m.baseCtx, session, timeoutMs)

	logger.Info("IP forward TCP listener opened", map[string]any{
		"sessionId":  sessionID,
		"localPort":  addr.Port,
		"mode":       string(params.Mode),
		"targetPort": params.Port,
	})

	return &OpenResult{
		SessionID:  sessionID,
		LocalHost:  "127.0.0.1",
		LocalPort:  addr.Port,
		Protocol:   "tcp",
		TargetPort: params.Port,
		Mode:       string(params.Mode),
	}, nil
}

func (m *Manager) acceptLoop(ctx context.Context, session *Session, timeoutMs int64) {
	for {
		conn, err := session.tcpListener.Accept()
		if err != nil {
			return
		}
		if err := m.handleTCPAccept(ctx, session, conn, timeoutMs); err != nil {
			logger.Warn("IP forward TCP accept failed", map[string]any{
				"sessionId": session.id,
				"error":     err.Error(),
			})
			_ = conn.Close()
		}
	}
}

func (m *Manager) handleTCPAccept(ctx context.Context, session *Session, conn net.Conn, timeoutMs int64) error {
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	if len(session.bridges) >= MaxTCPBridgesPerSession {
		session.mu.Unlock()
		return fmt.Errorf("Too many TCP bridges for session (max %d)", MaxTCPBridgesPerSession)
	}
	session.mu.Unlock()

	pc := m.getPC()
	if pc == nil {
		return fmt.Errorf("WebRTC peer connection is unavailable")
	}

	dataChannel, resolvedHost, err := m.startIpForwardRPC(m.baseCtx, session.mode, session.targetHost, session.targetPort, TCP, timeoutMs)
	if err != nil {
		return err
	}
	if resolvedHost != "" {
		session.mu.Lock()
		session.resolvedHost = resolvedHost
		session.mu.Unlock()
	}

	ordered := true
	channel, err := pc.CreateDataChannel(dataChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	b := &bridge{channel: channel, conn: conn}
	session.mu.Lock()
	session.bridges[b] = struct{}{}
	session.mu.Unlock()

	closeBridge := func() {
		session.mu.Lock()
		if b.closed {
			session.mu.Unlock()
			return
		}
		b.closed = true
		delete(session.bridges, b)
		session.mu.Unlock()
		_ = conn.Close()
		_ = channel.Close()
	}

	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		session.mu.Lock()
		closed := b.closed
		session.mu.Unlock()
		if closed {
			return
		}
		_, _ = conn.Write(msg.Data)
	})
	channel.OnClose(closeBridge)
	channel.OnError(func(error) { closeBridge() })

	// Serialize DC sends so concurrent reads cannot reorder the TCP stream.
	var sendMu sync.Mutex
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				session.mu.Lock()
				closed := b.closed
				session.mu.Unlock()
				if closed {
					return
				}
				if err := waitBufferedLow(channel); err != nil {
					closeBridge()
					return
				}
				sendMu.Lock()
				sendErr := channel.Send(buf[:n])
				sendMu.Unlock()
				if sendErr != nil {
					closeBridge()
					return
				}
			}
			if err != nil {
				closeBridge()
				return
			}
		}
	}()

	if err := waitChannelOpen(channel, timeoutMs); err != nil {
		closeBridge()
		return err
	}
	return nil
}

func (m *Manager) openUDP(ctx context.Context, sessionID string, params OpenParams, timeoutMs int64) (*OpenResult, error) {
	pc := m.getPC()
	if pc == nil {
		return nil, fmt.Errorf("WebRTC peer connection is unavailable")
	}
	dataChannel, resolvedHost, err := m.startIpForwardRPC(m.baseCtx, params.Mode, strings.TrimSpace(params.Host), params.Port, UDP, timeoutMs)
	if err != nil {
		return nil, err
	}
	ordered := true
	channel, err := pc.CreateDataChannel(dataChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, err
	}
	if err := waitChannelOpen(channel, timeoutMs); err != nil {
		return nil, err
	}

	localPort := 0
	if params.LocalPort != nil {
		localPort = *params.LocalPort
	}
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: localPort})
	if err != nil {
		_ = channel.Close()
		return nil, err
	}

	session := &Session{
		id:           sessionID,
		mode:         params.Mode,
		protocol:     UDP,
		targetHost:   strings.TrimSpace(params.Host),
		targetPort:   params.Port,
		resolvedHost: resolvedHost,
		localHost:    "127.0.0.1",
		localPort:    udpConn.LocalAddr().(*net.UDPAddr).Port,
		udpConn:      udpConn,
		udpChannel:   channel,
		bridges:      make(map[*bridge]struct{}),
	}
	b := &bridge{channel: channel}
	session.mu.Lock()
	session.bridges[b] = struct{}{}
	session.mu.Unlock()
	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	closeAll := func() {
		_ = m.disposeSession(session)
	}

	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		session.mu.Lock()
		closed := session.closed
		peer := session.udpPeer
		session.mu.Unlock()
		if closed || peer == nil {
			return
		}
		_, _ = udpConn.WriteToUDP(msg.Data, peer)
	})
	channel.OnClose(closeAll)
	channel.OnError(func(error) { closeAll() })

	go func() {
		buf := make([]byte, 65536)
		for {
			n, raddr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				closeAll()
				return
			}
			session.mu.Lock()
			closed := session.closed
			session.mu.Unlock()
			if closed || channel.ReadyState() != webrtc.DataChannelStateOpen {
				continue
			}
			if n > UDPMaxDatagram {
				logger.Warn("Dropping oversized local UDP datagram", map[string]any{"len": n})
				continue
			}
			session.mu.Lock()
			session.udpPeer = raddr
			session.mu.Unlock()
			if err := waitBufferedLow(channel); err != nil {
				closeAll()
				return
			}
			if err := channel.Send(buf[:n]); err != nil {
				closeAll()
				return
			}
		}
	}()

	logger.Info("IP forward UDP listener opened", map[string]any{
		"sessionId":    sessionID,
		"localPort":    session.localPort,
		"mode":         string(params.Mode),
		"resolvedHost": resolvedHost,
		"targetPort":   params.Port,
	})

	return &OpenResult{
		SessionID:    sessionID,
		LocalHost:    "127.0.0.1",
		LocalPort:    session.localPort,
		Protocol:     "udp",
		TargetPort:   params.Port,
		Mode:         string(params.Mode),
		ResolvedHost: resolvedHost,
	}, nil
}

func (m *Manager) disposeSession(session *Session) error {
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil
	}
	session.closed = true
	m.mu.Lock()
	delete(m.sessions, session.id)
	m.mu.Unlock()
	bridges := make([]*bridge, 0, len(session.bridges))
	for b := range session.bridges {
		b.closed = true
		bridges = append(bridges, b)
	}
	session.bridges = make(map[*bridge]struct{})
	listener := session.tcpListener
	udpConn := session.udpConn
	udpChannel := session.udpChannel
	session.mu.Unlock()

	for _, b := range bridges {
		if b.conn != nil {
			_ = b.conn.Close()
		}
		if b.channel != nil {
			_ = b.channel.Close()
		}
	}
	if listener != nil {
		_ = listener.Close()
	}
	if udpConn != nil {
		_ = udpConn.Close()
	}
	if udpChannel != nil {
		_ = udpChannel.Close()
	}
	return nil
}

func waitChannelOpen(channel *webrtc.DataChannel, timeoutMs int64) error {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for channel.ReadyState() != webrtc.DataChannelStateOpen {
		if time.Now().After(deadline) {
			return fmt.Errorf("IP forward DataChannel did not open")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

// waitBufferedLow polls the channel buffered amount below the high watermark
// (pion's OnBufferedAmountLow is per-threshold global, so polling is used
// instead).
func waitBufferedLow(channel *webrtc.DataChannel) error {
	for {
		if channel.BufferedAmount() <= DCHighWatermark {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var reCodeToken = regexp.MustCompile(`(?i)\[([a-z0-9_]+)\]`)

// ClassifiedError is the structured ip-forward failure hint.
type ClassifiedError struct {
	Message                          string `json:"message"`
	Code                             string `json:"code,omitempty"`
	HintEnableUsbNetwork             bool   `json:"hintEnableUsbNetwork,omitempty"`
	HintHostFallbackNeedsUserConfirm bool   `json:"hintHostFallbackNeedsUserConfirm,omitempty"`
}

// ClassifyError parses device/MCP errors into structured hints for agents.
func ClassifyError(err error) ClassifiedError {
	message := ""
	if err != nil {
		message = err.Error()
	}
	code := ""
	if m := reCodeToken.FindStringSubmatch(message); m != nil {
		code = m[1]
	}
	usbFail := code == "usb_network_disabled" || code == "usb_peer_not_leased"
	if !usbFail {
		usbRe := regexp.MustCompile(`(?i)usb network|usb dhcp|usb peer`)
		usbFail = usbRe.MatchString(message)
	}
	return ClassifiedError{
		Message:                          message,
		Code:                             code,
		HintEnableUsbNetwork:             usbFail,
		HintHostFallbackNeedsUserConfirm: usbFail,
	}
}
