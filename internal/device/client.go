// Package device implements the DeviceClient: device HTTP (status, password
// login), the WebRTC session lifecycle, HID input, virtual media / virtual
// USB / ATX helpers, storage uploads, and the HDMI capture/compare pipelines.
package device

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/forwarding"
	"arkkvm-mcp/internal/logger"
	"arkkvm-mcp/internal/media"
	"arkkvm-mcp/internal/protocol"
	"arkkvm-mcp/internal/toolerr"
	"arkkvm-mcp/internal/upload"
	"arkkvm-mcp/internal/webrtcsession"
)

const (
	defaultTimeoutMs = 30_000
	loginTimeoutMs   = 10_000
	mcpClientType    = "agent"
)

// Options configure the device client (no env fallback: the MCP server
// seeds options from the config store).
type Options struct {
	AgentCookie    string
	AgentCsrfToken string
	Password       string
	IceServersJSON string
	CloudURL       string
	CloudToken     string
	CloudDeviceID  string
}

// Client owns one device connection (settings fingerprint bound).
type Client struct {
	baseURL string

	credMu           sync.Mutex
	cookie           string
	csrfToken        string
	password         string
	iceServers       []config.IceServer
	cloudMode        bool
	cloudBaseURL     string
	cloudToken       string
	cloudDeviceID    string
	hasConfiguredIce bool

	session *webrtcsession.Session

	videoMu      sync.Mutex
	videoTrack   *webrtc.TrackRemote
	videoWaiters []chan *webrtc.TrackRemote

	pumpMu      sync.Mutex
	pumpRunning bool
	subscribers map[int]*videoSubscription
	nextSubID   int
	lastRtpAtMs atomic.Int64

	screenshotMu     sync.Mutex
	hubMu            sync.Mutex
	hdmiFrameHub     *media.PNGFrameHub
	hubSubscription  *videoSubscription
	hdmiFrameSources map[*media.WebRtcFrameSource]struct{}
	sourceSubs       map[*media.WebRtcFrameSource]*videoSubscription

	uploadsMu     sync.Mutex
	activeUploads map[*uploadHandle]struct{}

	ipForward *forwarding.Manager

	controlMu           sync.Mutex
	controlOwner        *string
	controlPending      *protocol.ControlPending
	lastControlConflict protocol.ControlState
}

// NewClient builds a client and its WebRTC session.
func NewClient(baseURL string, opts Options) *Client {
	c := &Client{
		hdmiFrameSources: make(map[*media.WebRtcFrameSource]struct{}),
		sourceSubs:       make(map[*media.WebRtcFrameSource]*videoSubscription),
		activeUploads:    make(map[*uploadHandle]struct{}),
		subscribers:      make(map[int]*videoSubscription),
	}
	if baseURL != "" {
		c.baseURL = strings.TrimSuffix(baseURL, "/") + "/"
	}
	c.cookie = opts.AgentCookie
	c.csrfToken = opts.AgentCsrfToken
	c.password = opts.Password
	iceServers, err := config.ParseIceServers(opts.IceServersJSON)
	if err != nil {
		logger.Warn("Ignoring invalid iceServersJson", map[string]any{"error": err.Error()})
		iceServers = nil
	}
	c.iceServers = iceServers
	c.hasConfiguredIce = opts.IceServersJSON != ""
	c.cloudToken = opts.CloudToken
	c.cloudDeviceID = opts.CloudDeviceID
	c.cloudMode = opts.CloudURL != "" && c.cloudToken != "" && c.cloudDeviceID != ""
	if opts.CloudURL != "" {
		c.cloudBaseURL = strings.TrimSuffix(opts.CloudURL, "/") + "/"
	}
	signalBase := c.cloudBaseURL
	if signalBase == "" {
		signalBase = c.baseURL
	}

	session := webrtcsession.New(webrtcsession.Deps{
		BaseURL:    signalBase,
		IceServers: c.iceServers,
		ClientType: mcpClientType,
		TargetDeviceID: func() string {
			if c.cloudMode {
				return c.cloudDeviceID
			}
			return ""
		},
		GetCredentials: func() webrtcsession.Credentials {
			if c.cloudMode {
				return webrtcsession.Credentials{Token: c.cloudToken}
			}
			c.credMu.Lock()
			defer c.credMu.Unlock()
			return webrtcsession.Credentials{Cookie: c.cookie, CSRFToken: c.csrfToken}
		},
		RefreshCredentials: func() (bool, error) {
			if c.cloudMode || c.password == "" {
				return false, nil
			}
			return c.loginWithPassword(context.Background())
		},
		ResolveIceServers: func() ([]config.IceServer, error) {
			if !c.cloudMode || c.hasConfiguredIce {
				return nil, nil
			}
			return c.fetchCloudIceServers(context.Background(), 10_000)
		},
		OnVideoTrack: c.onVideoTrack,
		OnRPCNotification: func(method string, params any) {
			c.handleRPCNotification(method, params)
		},
	})
	c.session = session
	session.OnDead(func(event webrtcsession.DeadEvent) {
		c.reclaimSessionChildren()
	})
	c.ipForward = forwarding.NewManager(
		func() *webrtc.PeerConnection {
			pc, err := c.session.GetPeerConnection()
			if err != nil {
				return nil
			}
			return pc
		},
		func(ctx context.Context, method string, params any, timeoutMs int64) (any, error) {
			return c.session.RPC(ctx, method, params, timeoutMs)
		},
	)
	logger.Debug("Device client initialized", map[string]any{
		"baseUrl":        signalBase,
		"transport":      transportName(c.cloudMode),
		"cloudDeviceId":  c.cloudDeviceID,
		"authenticated":  c.cookie != "" || c.csrfToken != "" || c.cloudToken != "",
		"passwordLogin":  c.password != "",
		"clientType":     mcpClientType,
		"iceServerCount": len(c.iceServers),
	})
	return c
}

func transportName(cloudMode bool) string {
	if cloudMode {
		return "cloud-relay"
	}
	return "device-direct"
}

// Session exposes the WebRTC session for connection snapshots.
func (c *Client) Session() *webrtcsession.Session { return c.session }

// IPForward exposes the forwarding manager.
func (c *Client) IPForward() *forwarding.Manager { return c.ipForward }

// CloudIceServers resolves cloud TURN config (cloud mode only).
func (c *Client) CloudIceServers(ctx context.Context, timeoutMs int64) ([]config.IceServer, error) {
	if !c.cloudMode {
		return nil, nil
	}
	return c.fetchCloudIceServers(ctx, timeoutMs)
}

func (c *Client) fetchCloudIceServers(ctx context.Context, timeoutMs int64) ([]config.IceServer, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	endpoint := c.cloudBaseURL + "webrtc/ice_config"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cloudToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Cloud ICE config failed: HTTP %d", response.StatusCode)
	}
	var body struct {
		Data *struct {
			IceServers json.RawMessage `json:"iceServers"`
		} `json:"data"`
		IceServers json.RawMessage `json:"iceServers"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, err
	}
	raw := body.Data.IceServers
	if len(raw) == 0 {
		raw = body.IceServers
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// The cloud returns a single RTCIceServer object or occasionally an array.
	if raw[0] == '[' {
		var servers []config.IceServer
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, err
		}
		return servers, nil
	}
	var server config.IceServer
	if err := json.Unmarshal(raw, &server); err != nil {
		return nil, err
	}
	return []config.IceServer{server}, nil
}

// GetConnectionStatus returns the WebRTC session snapshot.
func (c *Client) GetConnectionStatus() webrtcsession.Status {
	return c.session.Status()
}

func (c *Client) reclaimSessionChildren() {
	c.hubMu.Lock()
	hub := c.hdmiFrameHub
	c.hdmiFrameHub = nil
	hubSub := c.hubSubscription
	c.hubSubscription = nil
	sources := c.hdmiFrameSources
	subs := c.sourceSubs
	c.hdmiFrameSources = make(map[*media.WebRtcFrameSource]struct{})
	c.sourceSubs = make(map[*media.WebRtcFrameSource]*videoSubscription)
	c.hubMu.Unlock()
	if hub != nil {
		hub.Close()
	}
	if hubSub != nil {
		c.unsubscribeVideoPackets(hubSub)
	}
	for source := range sources {
		source.Close()
	}
	for _, sub := range subs {
		c.unsubscribeVideoPackets(sub)
	}
	c.closeAllSubscriptions()

	c.interruptUploads(fmt.Errorf("Device connection lost"))

	c.videoMu.Lock()
	c.videoTrack = nil
	waiters := c.videoWaiters
	c.videoWaiters = nil
	c.videoMu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

// Status reads the device HTTP status (device mode only; cloud mode returns
// the synthetic relay note).
func (c *Client) Status(ctx context.Context, timeoutMs int64) (any, error) {
	if c.cloudMode {
		return map[string]any{
			"transport":     "cloud-relay",
			"cloudUrl":      c.cloudBaseURL,
			"cloudDeviceId": c.cloudDeviceID,
			"note":          "Device-local HTTP (/device/status) is not reachable through the cloud relay; use kvm_rpc methods (getDeviceID, getVideoState, ...) for device facts.",
		}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"device/status", nil)
	if err != nil {
		return nil, err
	}
	c.credMu.Lock()
	cookie := c.cookie
	c.credMu.Unlock()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == 401 || response.StatusCode == 403 {
			return nil, toolerr.NewDeviceAuthError(response.StatusCode, "")
		}
		return nil, fmt.Errorf("Device status failed: HTTP %d", response.StatusCode)
	}
	var value any
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

var reAgentCookie = regexp.MustCompile(`(?:^|;\s*)authTokenAgent=([^;\s]+)`)

// loginWithPassword exchanges the configured device password for an
// agent-role session cookie and CSRF token (POST /auth/login-local). Returns
// false when the device answers 400 (password auth not enabled).
func (c *Client) loginWithPassword(ctx context.Context) (bool, error) {
	if c.password == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeoutMs*time.Millisecond)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"password": c.password, "clientType": mcpClientType})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"auth/login-local", strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == 400 {
		logger.Info("Password login rejected with 400; device likely runs without password auth", nil)
		return false, nil
	}
	if response.StatusCode == 401 {
		return false, toolerr.NewDeviceAuthError(401, "Device login failed: wrong password (HTTP 401)")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, fmt.Errorf("Device login failed: HTTP %d", response.StatusCode)
	}
	csrfHeader := response.Header.Get("x-csrf-token")
	var bodyJSON struct {
		CSRFToken string `json:"csrfToken"`
	}
	_ = json.NewDecoder(response.Body).Decode(&bodyJSON)
	cookie := extractAgentCookie(response)
	if cookie == "" {
		return false, fmt.Errorf("Device login response carried no authTokenAgent cookie")
	}
	c.credMu.Lock()
	c.cookie = cookie
	if csrfHeader != "" {
		c.csrfToken = csrfHeader
	} else {
		c.csrfToken = bodyJSON.CSRFToken
	}
	c.credMu.Unlock()
	logger.Info("Password login succeeded", map[string]any{"authenticated": true, "clientType": mcpClientType})
	return true, nil
}

func extractAgentCookie(response *http.Response) string {
	for _, entry := range response.Header.Values("Set-Cookie") {
		if m := reAgentCookie.FindStringSubmatch(entry); m != nil && m[1] != "" {
			return "authTokenAgent=" + m[1]
		}
	}
	return ""
}

// RPC performs one device JSON-RPC request over the control plane.
func (c *Client) RPC(ctx context.Context, method string, params any, timeoutMs int64) (any, error) {
	if timeoutMs <= 0 {
		timeoutMs = defaultTimeoutMs
	}
	return c.session.RPC(ctx, method, params, timeoutMs)
}

// Input sends one HID input report after ensuring this agent holds the
// HID control lease (requesting arbitration when a human currently owns it).
func (c *Client) Input(ctx context.Context, method string, params any) error {
	if err := c.ensureAgentControl(ctx); err != nil {
		return err
	}
	return c.session.SendInput(ctx, method, params)
}

// ---- Virtual media / ATX / virtual USB helpers ----

// GetVirtualMediaState reads the current virtual media state.
func (c *Client) GetVirtualMediaState(ctx context.Context) (any, error) {
	return c.session.RPC(ctx, "getVirtualMediaState", map[string]any{}, defaultTimeoutMs)
}

// GetVirtualMediaList lists the device image storage.
func (c *Client) GetVirtualMediaList(ctx context.Context) (any, error) {
	return c.session.RPC(ctx, "getVirtualMediaList", map[string]any{}, defaultTimeoutMs)
}

// MountVirtualMediaFromURL mounts an http(s) image.
func (c *Client) MountVirtualMediaFromURL(ctx context.Context, rawURL string, mode protocol.VirtualMediaMode, timeoutMs int64) error {
	safeURL, err := protocol.SanitizeHttpSourceUrl(rawURL)
	if err != nil {
		return err
	}
	_, err = c.session.RPC(ctx, "mountWithHTTP", map[string]any{"url": safeURL, "mode": string(mode)}, timeoutMs)
	return err
}

// MountVirtualMediaFromStorage mounts a previously uploaded image.
func (c *Client) MountVirtualMediaFromStorage(ctx context.Context, filename string, mode protocol.VirtualMediaMode, timeoutMs int64) error {
	safeFilename, err := protocol.SanitizeRemoteFilename(filename)
	if err != nil {
		return err
	}
	_, err = c.session.RPC(ctx, "mountWithStorage", map[string]any{"filename": safeFilename, "mode": string(mode)}, timeoutMs)
	return err
}

// UnmountVirtualMedia ejects the current image.
func (c *Client) UnmountVirtualMedia(ctx context.Context, timeoutMs int64) error {
	_, err := c.session.RPC(ctx, "unmountImage", map[string]any{}, timeoutMs)
	return err
}

// DeleteStorageFile removes a file from the device image storage. The device
// also cancels a matching in-flight upload and treats a missing file as a
// successful no-op.
func (c *Client) DeleteStorageFile(ctx context.Context, filename string, timeoutMs int64) error {
	safeFilename, err := protocol.SanitizeRemoteFilename(filename)
	if err != nil {
		return err
	}
	_, err = c.session.RPC(ctx, "deleteStorageFile", map[string]any{"filename": safeFilename}, timeoutMs)
	return err
}

// SetAtxPowerAction pulses the ATX power/reset lines.
func (c *Client) SetAtxPowerAction(ctx context.Context, action protocol.AtxPowerAction, timeoutMs int64) error {
	valid := false
	for _, a := range protocol.AtxPowerActions {
		if a == action {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("Invalid ATX power action: %s", string(action))
	}
	_, err := c.session.RPC(ctx, "setATXPowerAction", map[string]any{"action": string(action)}, timeoutMs)
	return err
}

// GetVirtualUsbState reads the file-transfer drive state.
func (c *Client) GetVirtualUsbState(ctx context.Context) (any, error) {
	return c.session.RPC(ctx, "getFileTransferState", map[string]any{}, defaultTimeoutMs)
}

// ListVirtualUsbFiles lists the file-transfer volume.
func (c *Client) ListVirtualUsbFiles(ctx context.Context, path string, timeoutMs int64) (any, error) {
	safePath, err := protocol.SanitizeVirtualUsbPath(path)
	if err != nil {
		return nil, err
	}
	result, err := c.session.RPC(ctx, "ftListFile", map[string]any{"path": "/" + safePath}, timeoutMs)
	if err != nil {
		return nil, err
	}
	if _, ok := result.([]any); !ok {
		return []any{}, nil
	}
	return result, nil
}

// DeleteVirtualUsbFile removes one file from the mounted file-transfer volume.
// Directories are intentionally not supported by this helper; the device has
// a separate ftDelDir RPC for them.
func (c *Client) DeleteVirtualUsbFile(ctx context.Context, path string, timeoutMs int64) error {
	safePath, err := protocol.SanitizeFtFilePath(path)
	if err != nil {
		return err
	}
	_, err = c.session.RPC(ctx, "ftDelFile", map[string]any{"path": safePath}, timeoutMs)
	return err
}

// MountVirtualUsbDrive switches the file-transfer drive target.
func (c *Client) MountVirtualUsbDrive(ctx context.Context, target protocol.VirtualUsbTarget, timeoutMs int64) error {
	valid := false
	for _, t := range protocol.VirtualUsbTargets {
		if t == target {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("Invalid virtual USB target: %s", string(target))
	}
	_, err := c.session.RPC(ctx, "mountWithFileImg", map[string]any{"target": string(target)}, timeoutMs)
	return err
}

// UnmountVirtualUsbDrive unmounts the file-transfer drive.
func (c *Client) UnmountVirtualUsbDrive(ctx context.Context, timeoutMs int64) error {
	_, err := c.session.RPC(ctx, "unmountWithFileImg", map[string]any{}, timeoutMs)
	return err
}

// ---- IP forwarding passthrough ----

// OpenIPForward opens a forwarding session.
func (c *Client) OpenIPForward(ctx context.Context, params forwarding.OpenParams, timeoutMs int64) (*forwarding.OpenResult, error) {
	if err := c.session.EnsureConnected(ctx); err != nil {
		return nil, err
	}
	return c.ipForward.Open(ctx, params, timeoutMs)
}

// IPForwardStatus reads one session's status.
func (c *Client) IPForwardStatus(sessionID string) (*forwarding.SessionStatus, error) {
	return c.ipForward.Status(sessionID)
}

// CloseIPForward closes one session.
func (c *Client) CloseIPForward(sessionID string) error {
	return c.ipForward.Close(sessionID)
}

// ---- Uploads ----

type uploadHandle struct {
	interrupt func(error)
}

func (c *Client) interruptUploads(err error) {
	c.uploadsMu.Lock()
	handles := make([]*uploadHandle, 0, len(c.activeUploads))
	for h := range c.activeUploads {
		handles = append(handles, h)
	}
	c.activeUploads = make(map[*uploadHandle]struct{})
	c.uploadsMu.Unlock()
	for _, h := range handles {
		func() {
			defer func() { _ = recover() }() // upload cleanup must never break teardown
			if h.interrupt != nil {
				h.interrupt(err)
			}
		}()
	}
}

// UploadResult is the shared storage/FT upload outcome.
type UploadResult struct {
	Filename         string `json:"filename"`
	Size             int64  `json:"size"`
	UploadedBytes    int64  `json:"uploadedBytes"`
	ResumedFromBytes int64  `json:"resumedFromBytes"`
	Complete         bool   `json:"complete"`
	DurationMs       int64  `json:"durationMs"`
	UploadID         string `json:"uploadId"`
}

func validateUploadSource(localPath, kind string) (int64, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s upload source is not a regular file: %s", kind, localPath)
	}
	if info.Size() <= 0 {
		return 0, fmt.Errorf("%s upload refuses empty files", kind)
	}
	if info.Size() > upload.MaxBytes {
		return 0, fmt.Errorf("%s upload file exceeds the %d-byte limit: %s", kind, upload.MaxBytes, localPath)
	}
	return info.Size(), nil
}

// UploadStorageFile streams a local file into the device image storage.
func (c *Client) UploadStorageFile(ctx context.Context, options UploadOptions) (*UploadResult, error) {
	return c.upload(ctx, options, uploadTarget{
		kind:      "Storage",
		rpcMethod: "startStorageFileUpload",
		validate:  protocol.ValidateStorageUploadStart,
		rpcParams: func(size int64, filename, remotePath string) any {
			return map[string]any{"filename": filename, "size": size}
		},
	})
}

// UploadFtFile streams a local file onto the file-transfer volume.
func (c *Client) UploadFtFile(ctx context.Context, options UploadOptions) (*UploadResult, error) {
	return c.upload(ctx, options, uploadTarget{
		kind:            "File-transfer",
		rpcMethod:       "startFtFsUpload",
		validate:        protocol.ValidateFtUploadStart,
		needsRemotePath: true,
		rpcParams: func(size int64, filename, remotePath string) any {
			return map[string]any{"path": remotePath, "name": filename, "size": size}
		},
	})
}

// UploadOptions are the shared upload parameters.
type UploadOptions struct {
	LocalPath      string
	RemoteFilename string
	RemotePath     string
	TimeoutMs      int64
}

type uploadTarget struct {
	kind            string
	rpcMethod       string
	validate        func(json.RawMessage, int64) (protocol.StorageUploadStartResponse, error)
	rpcParams       func(size int64, filename, remotePath string) any
	needsRemotePath bool
}

func (c *Client) upload(ctx context.Context, options UploadOptions, target uploadTarget) (*UploadResult, error) {
	startedAt := time.Now()
	timeoutMs := options.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = upload.DefaultTimeoutMs
	}
	deadline := startedAt.Add(time.Duration(timeoutMs) * time.Millisecond)
	totalBytes, err := validateUploadSource(options.LocalPath, target.kind)
	if err != nil {
		return nil, err
	}
	filename := options.RemoteFilename
	if filename == "" {
		filename = filepath.Base(options.LocalPath)
	}
	safeFilename, err := protocol.SanitizeRemoteFilename(filename)
	if err != nil {
		return nil, err
	}
	remotePath := "/"
	if target.needsRemotePath {
		remotePath, err = protocol.SanitizeFtDirectoryPath(firstNonEmpty(options.RemotePath, "/"))
		if err != nil {
			return nil, err
		}
	}
	logger.Info(fmt.Sprintf("%s upload starting", target.kind), map[string]any{
		"filename": safeFilename, "remotePath": remotePath, "size": totalBytes, "localPath": options.LocalPath,
	})

	remainingMs := func() int64 {
		r := time.Until(deadline).Milliseconds()
		if r <= 0 {
			r = 1
		}
		return r
	}
	response, err := c.session.RPC(ctx, target.rpcMethod, target.rpcParams(totalBytes, safeFilename, remotePath), remainingMs())
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	start, err := target.validate(raw, totalBytes)
	if err != nil {
		return nil, err
	}
	label := safeFilename
	if target.needsRemotePath {
		label = remotePath + "/" + safeFilename
	}
	return c.streamFileOverUploadChannel(ctx, streamArgs{
		localPath:  options.LocalPath,
		totalBytes: totalBytes,
		start:      start,
		label:      label,
		kind:       target.kind,
		startedAt:  startedAt,
		timeoutMs:  timeoutMs,
		deadline:   deadline,
	})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type streamArgs struct {
	localPath  string
	totalBytes int64
	start      protocol.StorageUploadStartResponse
	label      string
	kind       string
	startedAt  time.Time
	timeoutMs  int64
	deadline   time.Time
}

type uploadChannelState struct {
	mu          sync.Mutex
	closed      bool
	closeError  string
	totalBytes  int64
	ackedBytes  int64
	interrupted error
}

func (s *uploadChannelState) markClosed(closeErr string) {
	s.mu.Lock()
	s.closed = true
	if closeErr != "" && s.closeError == "" {
		s.closeError = closeErr
	}
	s.mu.Unlock()
}

func (s *uploadChannelState) acked() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ackedBytes
}

func (s *uploadChannelState) snapshot() (acked int64, closed bool, closeError string, interrupted error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ackedBytes, s.closed, s.closeError, s.interrupted
}

func (c *Client) streamFileOverUploadChannel(ctx context.Context, args streamArgs) (*UploadResult, error) {
	pc, err := c.session.GetPeerConnection()
	if err != nil {
		return nil, err
	}
	ordered := true
	channel, err := pc.CreateDataChannel(args.start.DataChannel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return nil, err
	}
	state := &uploadChannelState{totalBytes: args.totalBytes, ackedBytes: args.start.AlreadyUploadedBytes}

	handle := &uploadHandle{interrupt: func(interruptErr error) {
		state.mu.Lock()
		if interruptErr != nil && state.interrupted == nil {
			state.interrupted = interruptErr
		}
		state.mu.Unlock()
		state.markClosed("")
		_ = channel.Close()
	}}
	c.uploadsMu.Lock()
	c.activeUploads[handle] = struct{}{}
	c.uploadsMu.Unlock()

	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		ack := protocol.ParseUploadAck(msg.Data)
		if ack == nil {
			return
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if ack.Size != 0 && ack.Size != state.totalBytes {
			logger.Warn("Upload ack reported an unexpected total size", map[string]any{
				"upload": args.label, "expected": state.totalBytes, "reported": ack.Size,
			})
		}
		if ack.AlreadyUploadedBytes > state.ackedBytes {
			state.ackedBytes = ack.AlreadyUploadedBytes
		}
	})
	channel.OnClose(func() { state.markClosed("") })
	channel.OnError(func(e error) {
		msg := "upload channel error"
		if e != nil {
			msg = e.Error()
		}
		state.markClosed(msg)
	})

	defer func() {
		c.uploadsMu.Lock()
		delete(c.activeUploads, handle)
		c.uploadsMu.Unlock()
		state.markClosed("")
		_ = channel.Close()
	}()

	if err := waitForUploadChannelOpen(channel, state, args.deadline); err != nil {
		return nil, err
	}
	offset := state.acked()

	file, err := os.Open(args.localPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	assertAlive := func(sentBytes int64) error {
		_, closed, closeError, interrupted := state.snapshot()
		if interrupted != nil {
			return interrupted
		}
		if closed {
			msg := fmt.Sprintf("%s upload channel closed after %d/%d bytes", args.kind, sentBytes, args.totalBytes)
			if closeError != "" {
				msg += fmt.Sprintf(": %s", closeError)
			}
			msg += ". Rerun the same upload to resume."
			return fmt.Errorf("%s", msg)
		}
		return nil
	}

	buf := make([]byte, upload.ChunkSize)
	lastProgressLogAt := time.Now()
	for {
		if time.Now().After(args.deadline) {
			return nil, fmt.Errorf("%s upload timed out after %dms", args.kind, args.timeoutMs)
		}
		if err := assertAlive(offset); err != nil {
			return nil, err
		}
		if err := waitForUploadCapacity(channel, state, args.deadline); err != nil {
			return nil, err
		}
		if err := waitForUploadCredit(state, offset, args.deadline); err != nil {
			return nil, err
		}
		if err := assertAlive(offset); err != nil {
			return nil, err
		}
		n, readErr := file.Read(buf)
		if n > 0 {
			if err := channel.Send(buf[:n]); err != nil {
				return nil, err
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if time.Since(lastProgressLogAt) >= 10_000 {
			lastProgressLogAt = time.Now()
			elapsedS := time.Since(args.startedAt).Seconds()
			if elapsedS < 1 {
				elapsedS = 1
			}
			logger.Info("Upload progress", map[string]any{
				"upload":         args.label,
				"sentBytes":      offset,
				"ackedBytes":     state.acked(),
				"totalBytes":     args.totalBytes,
				"percent":        int(float64(offset) / float64(args.totalBytes) * 100),
				"bytesPerSecond": int(float64(offset) / elapsedS),
			})
		}
	}

	completionDeadline := args.deadline
	stall := time.Duration(upload.ComputeAckStallTimeout(args.totalBytes)) * time.Millisecond
	if stallEnd := time.Now().Add(stall); stallEnd.Before(completionDeadline) {
		completionDeadline = stallEnd
	}
	for {
		acked, closed, _, interrupted := state.snapshot()
		if interrupted != nil {
			return nil, interrupted
		}
		if acked >= args.totalBytes || closed || !time.Now().Before(completionDeadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	acked := state.acked()
	if acked >= args.totalBytes {
		logger.Info("Upload completed", map[string]any{
			"upload": args.label, "size": args.totalBytes, "durationMs": time.Since(args.startedAt).Milliseconds(),
		})
		return &UploadResult{
			Filename:         args.label,
			Size:             args.totalBytes,
			UploadedBytes:    acked,
			ResumedFromBytes: args.start.AlreadyUploadedBytes,
			Complete:         true,
			DurationMs:       time.Since(args.startedAt).Milliseconds(),
			UploadID:         args.start.DataChannel,
		}, nil
	}
	_, _, closeError, _ := state.snapshot()
	msg := fmt.Sprintf("%s upload incomplete: %d/%d bytes acknowledged", args.kind, acked, args.totalBytes)
	if closeError != "" {
		msg += fmt.Sprintf(" (channel error: %s)", closeError)
	}
	msg += fmt.Sprintf(". Rerun the same upload to resume from byte %d.", acked)
	return nil, fmt.Errorf("%s", msg)
}

func waitForUploadChannelOpen(channel *webrtc.DataChannel, state *uploadChannelState, deadline time.Time) error {
	for channel.ReadyState() != webrtc.DataChannelStateOpen {
		_, closed, closeError, _ := state.snapshot()
		if closed {
			msg := "Storage upload data channel closed before opening"
			if closeError != "" {
				msg = closeError
			}
			return fmt.Errorf("%s", msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Storage upload data channel did not open")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

func waitForUploadCredit(state *uploadChannelState, sentBytes int64, deadline time.Time) error {
	for sentBytes-state.acked() >= upload.MaxInFlightBytes {
		_, closed, closeError, interrupted := state.snapshot()
		if interrupted != nil {
			return interrupted
		}
		if closed {
			msg := "Storage upload channel closed"
			if closeError != "" {
				msg = closeError
			}
			return fmt.Errorf("%s", msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Storage upload acknowledgement timeout")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

func waitForUploadCapacity(channel *webrtc.DataChannel, state *uploadChannelState, deadline time.Time) error {
	for {
		if channel.BufferedAmount() <= upload.HighWatermark {
			return nil
		}
		_, closed, closeError, _ := state.snapshot()
		if closed {
			msg := "Storage upload channel closed"
			if closeError != "" {
				msg = closeError
			}
			return fmt.Errorf("%s", msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Storage upload backpressure timeout")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ---- HDMI capture ----

// CaptureHdmiScreenshot grabs one PNG still (serialized).
func (c *Client) CaptureHdmiScreenshot(ctx context.Context, timeoutMs int64) ([]byte, error) {
	startedAt := time.Now()
	logger.Debug("HDMI screenshot started", map[string]any{"timeoutMs": timeoutMs})
	c.screenshotMu.Lock()
	image, err := c.captureHdmiScreenshotUnlocked(ctx, timeoutMs)
	c.screenshotMu.Unlock()
	if err != nil {
		logger.Error("HDMI screenshot failed", err, map[string]any{"durationMs": time.Since(startedAt).Milliseconds()})
		return nil, err
	}
	logger.Info("HDMI screenshot captured", map[string]any{"bytes": len(image), "durationMs": time.Since(startedAt).Milliseconds()})
	return image, nil
}

func (c *Client) captureHdmiScreenshotUnlocked(ctx context.Context, timeoutMs int64) ([]byte, error) {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	if _, err := c.acquireLiveVideoTrack(ctx, time.Until(deadline).Milliseconds()); err != nil {
		return nil, err
	}
	c.hubMu.Lock()
	hub := c.hdmiFrameHub
	if hub == nil {
		subscription := c.subscribeVideoPackets()
		source, srcErr := media.NewWebRtcPNGSource(subscription.packets, subscription.closed)
		if srcErr != nil {
			c.hubMu.Unlock()
			c.unsubscribeVideoPackets(subscription)
			return nil, srcErr
		}
		hub = media.NewPNGFrameHub(source)
		c.hdmiFrameHub = hub
		c.hubSubscription = subscription
	}
	c.hubMu.Unlock()
	remaining := time.Until(deadline).Milliseconds()
	if remaining < 1 {
		remaining = 1
	}
	image, err := hub.Next(remaining)
	if err != nil {
		// A failed hub must not poison later screenshots.
		c.hubMu.Lock()
		if c.hdmiFrameHub == hub {
			c.hdmiFrameHub = nil
			if c.hubSubscription != nil {
				c.unsubscribeVideoPackets(c.hubSubscription)
				c.hubSubscription = nil
			}
		}
		c.hubMu.Unlock()
		return nil, err
	}
	if image == nil {
		return nil, fmt.Errorf("HDMI screenshot timed out after %dms", timeoutMs)
	}
	return image, nil
}

// CreateHdmiFrameSource opens a dedicated gray8 frame stream.
func (c *Client) CreateHdmiFrameSource(ctx context.Context, timeoutMs int64) (*media.WebRtcFrameSource, error) {
	if timeoutMs <= 0 {
		timeoutMs = 15_000
	}
	if _, err := c.acquireLiveVideoTrack(ctx, timeoutMs); err != nil {
		return nil, err
	}
	subscription := c.subscribeVideoPackets()
	source, err := media.NewWebRtcFrameSource(subscription.packets, subscription.closed)
	if err != nil {
		c.unsubscribeVideoPackets(subscription)
		return nil, err
	}
	c.hubMu.Lock()
	c.hdmiFrameSources[source] = struct{}{}
	c.sourceSubs[source] = subscription
	c.hubMu.Unlock()
	return source, nil
}

// ReleaseHdmiFrameSource closes and unregisters a frame source.
func (c *Client) ReleaseHdmiFrameSource(source *media.WebRtcFrameSource) {
	c.hubMu.Lock()
	_, ok := c.hdmiFrameSources[source]
	delete(c.hdmiFrameSources, source)
	sub := c.sourceSubs[source]
	delete(c.sourceSubs, source)
	c.hubMu.Unlock()
	if ok {
		source.Close()
	}
	if sub != nil {
		c.unsubscribeVideoPackets(sub)
	}
}

// CompareHdmiFrames captures before/after PNGs and diffs them.
func (c *Client) CompareHdmiFrames(ctx context.Context, delayMs int64, timeoutMs int64, threshold float64, region *media.Region) ([]byte, []byte, media.FrameComparison, error) {
	before, err := c.CaptureHdmiScreenshot(ctx, timeoutMs)
	if err != nil {
		return nil, nil, media.FrameComparison{}, err
	}
	select {
	case <-ctx.Done():
		return nil, nil, media.FrameComparison{}, ctx.Err()
	case <-time.After(time.Duration(delayMs) * time.Millisecond):
	}
	after, err := c.CaptureHdmiScreenshot(ctx, timeoutMs)
	if err != nil {
		return nil, nil, media.FrameComparison{}, err
	}
	comparison, err := media.ComparePNGFrames(before, after, threshold, 16, region)
	if err != nil {
		return nil, nil, media.FrameComparison{}, err
	}
	return before, after, comparison, nil
}

// WaitForHdmiOptions tune the change/stable waiters.
type WaitForHdmiOptions struct {
	TimeoutMs           int64
	IntervalMs          int64
	StableSamples       int
	ScreenshotTimeoutMs int64
	Threshold           float64
	Region              *media.Region
}

func (c *Client) captureBeforeDeadline(ctx context.Context, deadline time.Time, screenshotTimeoutMs int64) ([]byte, error) {
	remaining := time.Until(deadline).Milliseconds()
	if remaining <= 0 {
		return nil, fmt.Errorf("HDMI operation timed out")
	}
	if screenshotTimeoutMs <= 0 {
		screenshotTimeoutMs = 15_000
	}
	budget := remaining
	if screenshotTimeoutMs < budget {
		budget = screenshotTimeoutMs
	}
	return c.CaptureHdmiScreenshot(ctx, budget)
}

// WaitForHdmiChange polls screenshots until pixels change.
func (c *Client) WaitForHdmiChange(ctx context.Context, options WaitForHdmiOptions) (int64, media.FrameComparison, []byte, error) {
	timeoutMs := options.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 10_000
	}
	intervalMs := options.IntervalMs
	if intervalMs <= 0 {
		intervalMs = 300
	}
	startedAt := time.Now()
	deadline := startedAt.Add(time.Duration(timeoutMs) * time.Millisecond)
	previous, err := c.captureBeforeDeadline(ctx, deadline, options.ScreenshotTimeoutMs)
	if err != nil {
		return 0, media.FrameComparison{}, nil, err
	}
	lastComparison, err := media.UnchangedHdmiFrame(previous)
	if err != nil {
		return 0, media.FrameComparison{}, nil, err
	}
	for time.Now().Before(deadline) {
		if !sleepContext(ctx, min64(intervalMs, time.Until(deadline).Milliseconds())) {
			return 0, media.FrameComparison{}, nil, ctx.Err()
		}
		if !time.Now().Before(deadline) {
			break
		}
		current, err := c.captureBeforeDeadline(ctx, deadline, options.ScreenshotTimeoutMs)
		if err != nil {
			return 0, media.FrameComparison{}, nil, err
		}
		lastComparison, err = media.ComparePNGFrames(previous, current, options.Threshold, 16, options.Region)
		if err != nil {
			return 0, media.FrameComparison{}, nil, err
		}
		if lastComparison.Changed {
			return time.Since(startedAt).Milliseconds(), lastComparison, current, nil
		}
		previous = current
	}
	return time.Since(startedAt).Milliseconds(), lastComparison, previous, nil
}

// WaitForHdmiStable polls screenshots until the frame stops changing.
func (c *Client) WaitForHdmiStable(ctx context.Context, options WaitForHdmiOptions) (bool, int64, media.FrameComparison, []byte, error) {
	timeoutMs := options.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 10_000
	}
	intervalMs := options.IntervalMs
	if intervalMs <= 0 {
		intervalMs = 300
	}
	stableSamples := options.StableSamples
	if stableSamples <= 0 {
		stableSamples = 3
	}
	startedAt := time.Now()
	deadline := startedAt.Add(time.Duration(timeoutMs) * time.Millisecond)
	previous, err := c.captureBeforeDeadline(ctx, deadline, options.ScreenshotTimeoutMs)
	if err != nil {
		return false, 0, media.FrameComparison{}, nil, err
	}
	stableCount := 0
	lastComparison, err := media.UnchangedHdmiFrame(previous)
	if err != nil {
		return false, 0, media.FrameComparison{}, nil, err
	}
	for time.Now().Before(deadline) {
		if !sleepContext(ctx, min64(intervalMs, time.Until(deadline).Milliseconds())) {
			return false, 0, media.FrameComparison{}, nil, ctx.Err()
		}
		if !time.Now().Before(deadline) {
			break
		}
		current, err := c.captureBeforeDeadline(ctx, deadline, options.ScreenshotTimeoutMs)
		if err != nil {
			return false, 0, media.FrameComparison{}, nil, err
		}
		lastComparison, err = media.ComparePNGFrames(previous, current, options.Threshold, 16, options.Region)
		if err != nil {
			return false, 0, media.FrameComparison{}, nil, err
		}
		if lastComparison.Changed {
			stableCount = 0
		} else {
			stableCount++
		}
		previous = current
		if stableCount >= stableSamples {
			return true, time.Since(startedAt).Milliseconds(), lastComparison, current, nil
		}
	}
	return false, time.Since(startedAt).Milliseconds(), lastComparison, previous, nil
}

func min64(a, b int64) int64 {
	if b < a {
		return b
	}
	return a
}

// sleepContext sleeps for ms, returning false when ctx was cancelled first.
func sleepContext(ctx context.Context, ms int64) bool {
	if ms > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(ms) * time.Millisecond):
		}
	}
	select {
	case <-ctx.Done():
		return false
	default:
	}
	return true
}

// Close shuts the client down and reclaims all children.
func (c *Client) Close() {
	c.session.Close()
	c.reclaimSessionChildren()
	logger.Info("Device client closed", nil)
}

// acquireLiveVideoTrack waits for a video track and verifies it is
// delivering RTP. On a silent track: invalidate the WebRTC session and
// return an error — the Agent decides whether to retry.
func (c *Client) acquireLiveVideoTrack(ctx context.Context, timeoutMs int64) (*webrtc.TrackRemote, error) {
	if timeoutMs <= 0 {
		timeoutMs = 1
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	remaining := func() int64 {
		r := time.Until(deadline).Milliseconds()
		if r < 1 {
			r = 1
		}
		return r
	}
	connectCtx, cancelConnect := context.WithTimeout(ctx, time.Duration(remaining())*time.Millisecond)
	err := c.session.EnsureConnected(connectCtx)
	cancelConnect()
	if err != nil {
		return nil, fmt.Errorf("HDMI stream connection timed out")
	}
	track, err := c.waitVideoTrack(ctx, remaining())
	if err != nil {
		return nil, err
	}
	if track == nil {
		c.session.Invalidate("HDMI video track was not received")
		return nil, toolerr.NewConnectionError(toolerr.CodeConnectionLost, "HDMI video track was not received", 0)
	}
	if c.probeVideoTrackLive(remaining(), 3_000) {
		return track, nil
	}
	logger.Warn("HDMI video track is not receiving RTP; invalidating session", nil)
	c.session.Invalidate("HDMI video track received no RTP data (stale WebRTC connection)")
	return nil, toolerr.NewConnectionError(toolerr.CodeConnectionLost, "HDMI video track received no RTP data (stale WebRTC connection)", 0)
}

// probeVideoTrackLive reports whether RTP arrived within the probe window.
func (c *Client) probeVideoTrackLive(timeoutMs int64, maxProbe int64) bool {
	if timeoutMs > maxProbe {
		timeoutMs = maxProbe
	}
	if timeoutMs <= 0 {
		timeoutMs = 1
	}
	baseline := c.lastRtpAtMs.Load()
	if baseline != 0 && time.Since(time.UnixMilli(baseline)) < time.Second {
		// RTP seen very recently; no need to wait for another packet.
		return true
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		if c.lastRtpAtMs.Load() != baseline {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (c *Client) waitVideoTrack(ctx context.Context, timeoutMs int64) (*webrtc.TrackRemote, error) {
	c.videoMu.Lock()
	if c.videoTrack != nil {
		track := c.videoTrack
		c.videoMu.Unlock()
		return track, nil
	}
	ch := make(chan *webrtc.TrackRemote, 1)
	c.videoWaiters = append(c.videoWaiters, ch)
	c.videoMu.Unlock()
	if timeoutMs <= 0 {
		timeoutMs = 1
	}
	select {
	case track, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("Device connection lost")
		}
		return track, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil, nil
	}
}

func (c *Client) onVideoTrack(track *webrtc.TrackRemote) {
	c.videoMu.Lock()
	c.videoTrack = track
	waiters := c.videoWaiters
	c.videoWaiters = nil
	c.videoMu.Unlock()
	for _, w := range waiters {
		w <- track
	}
	c.startVideoPump(track)
	logger.Info("HDMI video track received", nil)
}

// videoSubscription is one decoder's packet feed; the pump fans packets out
// to every subscription.
type videoSubscription struct {
	id      int
	packets chan *rtp.Packet
	closed  chan struct{}
}

func (c *Client) startVideoPump(track *webrtc.TrackRemote) {
	c.pumpMu.Lock()
	if c.pumpRunning {
		c.pumpMu.Unlock()
		return
	}
	c.pumpRunning = true
	c.pumpMu.Unlock()

	go func() {
		defer func() {
			c.pumpMu.Lock()
			c.pumpRunning = false
			subs := make([]*videoSubscription, 0, len(c.subscribers))
			for _, sub := range c.subscribers {
				subs = append(subs, sub)
				delete(c.subscribers, sub.id)
			}
			c.pumpMu.Unlock()
			for _, sub := range subs {
				close(sub.closed)
			}
		}()
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			c.lastRtpAtMs.Store(time.Now().UnixMilli())
			c.pumpMu.Lock()
			subs := make([]chan *rtp.Packet, 0, len(c.subscribers))
			for _, sub := range c.subscribers {
				subs = append(subs, sub.packets)
			}
			c.pumpMu.Unlock()
			for _, packets := range subs {
				select {
				case packets <- packet:
				default:
					// Slow decoder: dropping breaks H264 assembly, but a
					// stalled pipe would otherwise block the whole track.
				}
			}
		}
	}()
}

func (c *Client) closeAllSubscriptions() {
	c.pumpMu.Lock()
	subs := make([]*videoSubscription, 0, len(c.subscribers))
	for _, sub := range c.subscribers {
		subs = append(subs, sub)
		delete(c.subscribers, sub.id)
	}
	c.pumpMu.Unlock()
	for _, sub := range subs {
		close(sub.closed)
	}
}

func (c *Client) subscribeVideoPackets() *videoSubscription {
	c.pumpMu.Lock()
	defer c.pumpMu.Unlock()
	c.nextSubID++
	sub := &videoSubscription{
		id:      c.nextSubID,
		packets: make(chan *rtp.Packet, 256),
		closed:  make(chan struct{}),
	}
	c.subscribers[sub.id] = sub
	return sub
}

func (c *Client) unsubscribeVideoPackets(sub *videoSubscription) {
	c.pumpMu.Lock()
	delete(c.subscribers, sub.id)
	c.pumpMu.Unlock()
}
