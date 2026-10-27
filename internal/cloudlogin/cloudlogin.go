// Package cloudlogin implements kvm_cloud_login support: derive the login
// URL, run a one-shot loopback listener that catches the OIDC redirect
// (/ad-login?code=…), exchange the one-time grant code for the dedicated MCP
// token (POST /mcp/token), and manage the optional 0600 token file. The token
// must be caught here because the cloud webapp stores it in browser
// localStorage, which this process cannot read.
//
// Two callback shapes share the listener:
//   - NEW (client=mcp grant flow): the login URL carries client=mcp&req=<random>;
//     the callback returns a one-time grant code bound to req — never the web
//     session — and the exchange yields the long-lived, revocable MCP token.
//   - LEGACY (old clouds): they redirect with token=/loginToken=; captured as
//     kind 'session' with an upgrade hint.
package cloudlogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/logger"
)

const (
	defaultLoginTimeoutMs = 300_000
	maxLoginTimeoutMs     = 600_000
	// LoginFW is the capability statement sent as the login page's fw param.
	LoginFW = "0.1.0"
)

// ManualTokenFallback is shown whenever a login flow cannot hand the token to
// the local callback: the deployed cloud keeps direct logins on its own app
// instead of redirecting to the loopback returnTo.
const ManualTokenFallback = "Manual token fallback: complete the sign-in in the browser (the cloud may " +
	"leave you on its own app, e.g. /devices/list — the session is still created). " +
	"On that signed-in cloud page open DevTools → Console and run " +
	"localStorage.getItem('token'), copy the value, then call " +
	"kvm_configure action=set cloudUrl=<cloud API: https://api.arkkvm.com (global) " +
	"or https://api.arkkvm.cn (China)> " +
	"cloudToken=<copied token> (cloudUrl is required together with cloudToken). " +
	"Finish with kvm_cloud_login action=devices and " +
	"kvm_configure action=set cloudDeviceId=<deviceId>."

var (
	reUnsafeControl = regexp.MustCompile(`[\x{0000}-\x{0020}\x{007f}-\x{009f}\\]`)
	reUnsafeEncoded = regexp.MustCompile(`(?i)%(?:0[0-9a-f]|1[0-9a-f]|7f|5c)`)
	reHTTPScheme    = regexp.MustCompile(`(?i)^https?://`)
	reAPIHost       = regexp.MustCompile(`(?i)^api\.`)
	reOIDCParam     = regexp.MustCompile(`^oidc([A-Za-z]+)$`)
)

// hasUnsafeURLInput mirrors arkkvm-app-frontend src/utils/security.ts.
func hasUnsafeURLInput(value string) bool {
	return reUnsafeControl.MatchString(value) || reUnsafeEncoded.MatchString(value)
}

// NormalizeHTTPOrigin does syntax-only http(s) origin validation: no
// userinfo, no path. Returns "" when invalid.
func NormalizeHTTPOrigin(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" || hasUnsafeURLInput(raw) || !reHTTPScheme.MatchString(raw) {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	origin := u.Scheme + "://" + u.Host
	return origin
}

// DeriveCloudAppOrigin resolves the login-page origin from the cloud API
// origin: api.<domain> clouds map to app.<domain>. Non-api ArkKVM hosts keep
// their region (arkkvm.com → app.arkkvm.com, arkkvm.cn → app.arkkvm.cn);
// other hosts fall back to the canonical https://app.arkkvm.com. An explicit
// appUrl always wins.
func DeriveCloudAppOrigin(cloudAPIURL, appURL string) string {
	apiOrigin := NormalizeHTTPOrigin(cloudAPIURL)
	if apiOrigin == "" {
		return ""
	}
	if appURL != "" {
		return NormalizeHTTPOrigin(appURL)
	}
	u, err := url.Parse(apiOrigin)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	derived := "app.arkkvm.com"
	switch {
	case reAPIHost.MatchString(host):
		derived = "app." + host[4:]
	case host == "arkkvm.cn" || strings.HasSuffix(host, ".arkkvm.cn"):
		derived = "app.arkkvm.cn"
	case host == "arkkvm.com" || strings.HasSuffix(host, ".arkkvm.com"):
		derived = "app.arkkvm.com"
	}
	return u.Scheme + "://" + derived
}

// FlowOptions configure one login flow.
type FlowOptions struct {
	CloudAPIURL string
	AppURL      string
	DeviceID    string
	TimeoutMs   int64
}

// Result settles the login flow.
type Result struct {
	Success  bool
	Token    string
	Kind     string // config.TokenKindMCP | config.TokenKindSession
	Provider string
	LoginURL string
	Error    string
	TimedOut bool
}

// ExchangeMcpGrantCode swaps a one-time grant code for the dedicated MCP
// token (POST /mcp/token, BaseResult envelope). The code is single-use and
// burned on every attempt — a failure here is terminal for this flow.
func ExchangeMcpGrantCode(ctx context.Context, cloudAPIURL, code, req string, timeoutMs int64) (string, error) {
	apiOrigin := NormalizeHTTPOrigin(cloudAPIURL)
	if apiOrigin == "" {
		return "", fmt.Errorf("cloudUrl must be a valid http(s) URL")
	}
	if timeoutMs <= 0 {
		timeoutMs = 15_000
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"code": code, "req": req})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiOrigin+"/mcp/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.NewDecoder(response.Body).Decode(&payload)
	if response.StatusCode < 200 || response.StatusCode >= 300 || !payload.Success || payload.Data.Token == "" {
		message := payload.Message
		if message == "" {
			message = fmt.Sprintf("HTTP %d", response.StatusCode)
		}
		return "", fmt.Errorf("POST /mcp/token rejected the grant code (%s). Re-run kvm_cloud_login action=login.", message)
	}
	return payload.Data.Token, nil
}

// ActiveFlow is a running login catcher.
type ActiveFlow struct {
	LoginURL string
	Done     chan Result
	cancel   func()
}

// Cancel aborts the flow early.
func (f *ActiveFlow) Cancel() {
	if f.cancel != nil {
		f.cancel()
	}
}

// StartFlow runs the loopback catcher and returns once it is listening.
// `done` settles on the first /ad-login callback or at timeout.
func StartFlow(ctx context.Context, options FlowOptions) (*ActiveFlow, error) {
	apiOrigin := NormalizeHTTPOrigin(options.CloudAPIURL)
	if apiOrigin == "" {
		return nil, fmt.Errorf("cloudUrl must be a valid http(s) URL")
	}
	appOrigin := DeriveCloudAppOrigin(apiOrigin, options.AppURL)
	if appOrigin == "" {
		return nil, fmt.Errorf("appUrl must be a valid http(s) URL")
	}
	timeoutMs := options.TimeoutMs
	if timeoutMs < 1_000 {
		timeoutMs = defaultLoginTimeoutMs
	}
	if timeoutMs > maxLoginTimeoutMs {
		timeoutMs = maxLoginTimeoutMs
	}

	listener, err := netListen("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*netTCPAddr).Port
	returnTo := fmt.Sprintf("http://127.0.0.1:%d", port)

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		_ = listener.Close()
		return nil, err
	}
	mcpReq := hex.EncodeToString(random)

	params := url.Values{}
	params.Set("returnTo", returnTo)
	params.Set("cloudApiUrl", apiOrigin)
	params.Set("fw", LoginFW)
	params.Set("client", "mcp")
	params.Set("req", mcpReq)
	if options.DeviceID != "" {
		params.Set("deviceId", options.DeviceID)
	}
	loginURL := appOrigin + "/login?" + params.Encode()
	logger.Info("Cloud login flow listening", map[string]any{"port": port, "timeoutMs": timeoutMs})

	flowCtx, cancel := context.WithCancel(ctx)
	done := make(chan Result, 1)
	flow := &ActiveFlow{LoginURL: loginURL, Done: done, cancel: func() {
		cancel()
	}}

	settle := func(result Result) {
		select {
		case done <- result:
		default:
		}
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ad-login" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			query := r.URL.Query()
			provider := deriveProviderFromCallback(query)
			grantCode := query.Get("code")
			if grantCode != "" {
				// NEW shape: one-time grant code → dedicated MCP token. The
				// binding must match the random this exact flow generated.
				if query.Get("req") != mcpReq {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>ArkKVM MCP</title><p>Callback req binding mismatch — go back to the login page and retry.</p>"))
					settle(Result{Success: false, LoginURL: loginURL, Error: "Grant code req binding mismatch"})
					cancel()
					return
				}
				mcpToken, exchangeErr := ExchangeMcpGrantCode(flowCtx, apiOrigin, grantCode, mcpReq, 15_000)
				if exchangeErr != nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte("<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>ArkKVM MCP</title><p>Grant code exchange failed: " + exchangeErr.Error() + "</p>"))
					settle(Result{Success: false, LoginURL: loginURL, Error: "Grant code exchange failed: " + exchangeErr.Error()})
					cancel()
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(successPage("ArkKVM MCP has received its dedicated access token. You can close this page.")))
				settle(Result{Success: true, Token: mcpToken, Kind: config.TokenKindMCP, Provider: provider, LoginURL: loginURL})
				cancel()
				return
			}
			// LEGACY shape (old clouds): loginToken is the web session
			// (pairing shape), token the direct-login fallback.
			token := query.Get("loginToken")
			if token == "" {
				token = query.Get("token")
			}
			if token == "" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("<!doctype html><html lang=\"en\"><meta charset=\"utf-8\"><title>ArkKVM MCP</title><p>Callback is missing the code/token parameter — go back to the login page and retry.</p>"))
				settle(Result{Success: false, LoginURL: loginURL, Error: "Callback carried no code or token/loginToken parameter"})
				cancel()
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(successPage("ArkKVM MCP has received the cloud session token (legacy cloud mode). You can close this page.")))
			settle(Result{Success: true, Token: token, Kind: config.TokenKindSession, Provider: provider, LoginURL: loginURL})
			cancel()
		}),
	}

	// Serve until the flow context is cancelled or the outer ctx ends.
	go func() {
		<-flowCtx.Done()
		_ = server.Close()
	}()
	go func() {
		_ = server.Serve(listener)
	}()
	go func() {
		select {
		case <-flowCtx.Done():
		case <-ctx.Done():
			settle(Result{Success: false, LoginURL: loginURL, Error: "Cloud login flow cancelled"})
			cancel()
			return
		case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
			settle(Result{
				Success:  false,
				LoginURL: loginURL,
				TimedOut: true,
				// status polling surfaces only this string, so it must carry
				// the manual fallback by itself.
				Error: fmt.Sprintf("Login callback not received within %ds. %s", int64(timeoutMs/1000), ManualTokenFallback),
			})
			cancel()
		}
	}()
	return flow, nil
}

func successPage(message string) string {
	return "<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>ArkKVM MCP</title></head>" +
		"<body style=\"font-family:system-ui;display:grid;place-items:center;height:100vh;margin:0\">" +
		"<div style=\"text-align:center\"><h2>✅ Authorization complete</h2>" +
		"<p>" + message + "</p></div></body></html>"
}

// deriveProviderFromCallback records only the provider name — never store or
// log the identity token itself.
func deriveProviderFromCallback(query url.Values) string {
	for key := range query {
		if m := reOIDCParam.FindStringSubmatch(key); m != nil {
			return strings.ToLower(m[1])
		}
	}
	return ""
}

// OpenInBrowser launches the system browser; false when unavailable.
func OpenInBrowser(loginURL string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", loginURL)
	case "linux":
		cmd = exec.Command("xdg-open", loginURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", loginURL)
	default:
		return false
	}
	if err := cmd.Start(); err != nil {
		logger.Warn("Failed to open browser for cloud login", map[string]any{"error": err.Error()})
		return false
	}
	go func() { _ = cmd.Wait() }()
	return true
}

// CloudDeviceRow is one account device.
type CloudDeviceRow struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name,omitempty"`
	Online   *bool  `json:"online,omitempty"`
	LastSeen string `json:"lastSeen,omitempty"`
}

// FetchCloudDevices lists the account's devices (GET {cloudUrl}/devices).
func FetchCloudDevices(ctx context.Context, cloudAPIURL, token string, timeoutMs int64) ([]CloudDeviceRow, error) {
	apiOrigin := NormalizeHTTPOrigin(cloudAPIURL)
	if apiOrigin == "" {
		return nil, fmt.Errorf("cloudUrl must be a valid http(s) URL")
	}
	if token == "" {
		return nil, fmt.Errorf("A cloud session token is required: run kvm_cloud_login action=login first.")
	}
	if timeoutMs <= 0 {
		timeoutMs = 15_000
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiOrigin+"/devices", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return nil, fmt.Errorf("Cloud session token rejected (HTTP %d): it may have expired. Re-authenticate with kvm_cloud_login action=login.", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Cloud device list failed: HTTP %d", response.StatusCode)
	}
	var body struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Success != nil && !*body.Success {
		return nil, fmt.Errorf("Cloud device list rejected the request")
	}
	var rows []map[string]any
	if len(body.Data) > 0 && string(body.Data) != "null" {
		if err := json.Unmarshal(body.Data, &rows); err != nil {
			rows = nil
		}
	}
	out := make([]CloudDeviceRow, 0, len(rows))
	for _, row := range rows {
		id := row["id"]
		if id == nil {
			id = row["deviceId"]
		}
		deviceID, _ := id.(string)
		if deviceID == "" {
			continue
		}
		entry := CloudDeviceRow{DeviceID: deviceID}
		if name, ok := row["name"].(string); ok {
			entry.Name = name
		}
		if online, ok := row["online"].(bool); ok {
			entry.Online = &online
		}
		if row["lastSeen"] != nil {
			entry.LastSeen = fmt.Sprintf("%v", row["lastSeen"])
		}
		out = append(out, entry)
	}
	return out, nil
}

// ResolveCloudDeviceName finds the name for a known deviceId (best effort).
func ResolveCloudDeviceName(ctx context.Context, cloudAPIURL, token, deviceID string, timeoutMs int64) (string, error) {
	if deviceID == "" {
		return "", nil
	}
	rows, err := FetchCloudDevices(ctx, cloudAPIURL, token, timeoutMs)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.DeviceID == deviceID {
			return row.Name, nil
		}
	}
	return "", nil
}

// ---- Managed login (kvm_cloud_login wait:false) ----

// ManagedDevicePickup surfaces post-apply device-list info.
type ManagedDevicePickup struct {
	Devices            []CloudDeviceRow `json:"devices,omitempty"`
	AutoPickedDeviceID string           `json:"autoPickedDeviceId,omitempty"`
}

// ManagedState is the status view of the managed login flow.
type ManagedState struct {
	Pending      bool                 `json:"pending"`
	LoginURL     string               `json:"loginUrl,omitempty"`
	StartedAt    string               `json:"startedAt,omitempty"`
	Success      *bool                `json:"success,omitempty"`
	Error        string               `json:"error,omitempty"`
	Applied      bool                 `json:"applied,omitempty"`
	DevicePickup *ManagedDevicePickup `json:"devicePickup,omitempty"`
}

// ApplyTokenFn applies a freshly captured token to the running config.
type ApplyTokenFn func(ctx context.Context, token, provider, kind string, persist bool) (*ManagedDevicePickup, error)

// Manager tracks one login flow at a time.
type Manager struct {
	mu      sync.Mutex
	apply   ApplyTokenFn
	current *managedFlow
}

type managedFlow struct {
	flow         *ActiveFlow
	startedAt    string
	outcome      *Result
	applied      bool
	devicePickup *ManagedDevicePickup
}

// NewManager builds the manager.
func NewManager(apply ApplyTokenFn) *Manager {
	return &Manager{apply: apply}
}

// IsActive reports whether a flow is pending.
func (m *Manager) IsActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current != nil && m.current.outcome == nil
}

// Start launches a background login flow.
func (m *Manager) Start(ctx context.Context, options FlowOptions, persist bool, openBrowser bool) (ManagedState, error) {
	m.mu.Lock()
	if m.current != nil && m.current.outcome == nil {
		state := ManagedState{
			Pending:   true,
			LoginURL:  m.current.flow.LoginURL,
			StartedAt: m.current.startedAt,
			Error:     "A login flow is already active; open its URL or wait for the callback.",
		}
		m.mu.Unlock()
		return state, nil
	}
	m.mu.Unlock()

	// Background login must outlive the MCP tool request context: Cursor (and
	// other hosts) cancel that ctx as soon as wait=false returns, which used
	// to abort the loopback listener with "Cloud login flow cancelled".
	flow, err := StartFlow(context.Background(), options)
	if err != nil {
		return ManagedState{}, err
	}
	startedAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	entry := &managedFlow{flow: flow, startedAt: startedAt}
	m.mu.Lock()
	m.current = entry
	m.mu.Unlock()
	if openBrowser {
		OpenInBrowser(flow.LoginURL)
	}
	go func() {
		outcome := <-flow.Done
		entry.outcome = &outcome
		if outcome.Success && outcome.Token != "" {
			pickup, applyErr := m.apply(context.Background(), outcome.Token, outcome.Provider, outcome.Kind, persist)
			if applyErr == nil {
				entry.devicePickup = pickup
				entry.applied = true
			} else {
				logger.Warn("Background cloud login apply failed", map[string]any{"error": applyErr.Error()})
			}
		}
	}()
	return ManagedState{Pending: true, LoginURL: flow.LoginURL, StartedAt: startedAt}, nil
}

// Status reports the managed flow state.
func (m *Manager) Status() ManagedState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return ManagedState{Pending: false}
	}
	entry := m.current
	state := ManagedState{
		Pending:   entry.outcome == nil,
		LoginURL:  entry.flow.LoginURL,
		StartedAt: entry.startedAt,
	}
	if entry.outcome != nil {
		success := entry.outcome.Success
		state.Success = &success
		state.Error = entry.outcome.Error
		state.Applied = entry.applied
		if success && entry.applied && entry.devicePickup != nil {
			state.DevicePickup = entry.devicePickup
		}
	}
	return state
}

// DevicePickupHint guides a freshly applied background-login token whose
// account still has no device target. Returns "" when no hint is needed.
func DevicePickupHint(pickup *ManagedDevicePickup, deviceConfigured bool) string {
	if deviceConfigured {
		return ""
	}
	if pickup == nil || pickup.Devices == nil {
		return "Token applied but the device list is unknown: run kvm_cloud_login action=devices to list the account devices, then pick the target and run kvm_configure action=set cloudDeviceId=<deviceId> to finish cloud relay setup."
	}
	if len(pickup.Devices) > 1 {
		return fmt.Sprintf("The account has %d devices: pick the target one and run kvm_configure action=set cloudDeviceId=<deviceId> to finish cloud relay setup; if it is unclear which device to use, ask the user.", len(pickup.Devices))
	}
	if len(pickup.Devices) == 1 {
		return fmt.Sprintf("One device on the account (%s): run kvm_configure action=set cloudDeviceId=%s to finish cloud relay setup.", pickup.Devices[0].DeviceID, pickup.Devices[0].DeviceID)
	}
	return "No devices on this account; pair one from the device web UI first."
}

// Silence unused imports in constrained builds.
var (
	_ = io.Discard
	_ = os.Getenv
)
