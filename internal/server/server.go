// Package server wires the MCP server: 30 tools, the in-memory config store,
// and the fingerprint-bound DeviceClient lifecycle (any settings change
// closes the current WebRTC session so the next call reconnects).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"arkkvm-mcp/internal/cloudlogin"
	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/device"
	"arkkvm-mcp/internal/logger"
)

// Version. Build scripts override it at link time with FIRMWARE_VERSION:
// -ldflags "-X arkkvm-mcp/internal/server.Version=<ver>"
var Version = "0.1.0"

// ServerName is the MCP server name reported at initialize.
const ServerName = "arkkvm-local-mcp"

// Server holds all shared state behind the tools.
type Server struct {
	Store *config.Store
	Log   *logger.Logger
	mcp   *mcp.Server

	cloudLogin *cloudlogin.Manager

	mu           sync.Mutex
	activeClient *clientEntry
}

type clientEntry struct {
	fingerprint string
	client      *device.Client
}

// New builds the server around a seeded config store.
func New(store *config.Store, log *logger.Logger) *Server {
	s := &Server{Store: store, Log: log}
	s.cloudLogin = cloudlogin.NewManager(s.applyManagedLoginToken)
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: Version}, &mcp.ServerOptions{})
	s.registerCoreTools()
	s.registerCloudLoginTool()
	s.registerForwardingTools()
	s.registerHdmiTools()
	s.registerInputTools()
	s.registerControlTools()
	s.registerMediaTools()
	return s
}

// MCP exposes the underlying protocol server.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Run serves MCP over stdio until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// CloudLoginManager exposes the background login manager for tests.
func (s *Server) CloudLoginManager() *cloudlogin.Manager { return s.cloudLogin }

// CloseClient releases the active device client, if any.
func (s *Server) CloseClient() {
	s.mu.Lock()
	entry := s.activeClient
	s.activeClient = nil
	s.mu.Unlock()
	if entry != nil {
		entry.client.Close()
	}
}

// getClient returns the DeviceClient for the current settings, rebuilding it
// whenever the settings fingerprint changed.
func (s *Server) getClient(tool string) (*device.Client, error) {
	settings := s.Store.Get()
	mode := config.ConnectionModeOf(settings)
	if mode == config.ModeDevice && settings.URL == "" {
		if settings.CloudToken != "" {
			return nil, fmt.Errorf(
				"%s has a cloud token but no target device. Run kvm_cloud_login action=devices to list "+
					"your devices, then kvm_configure action=set cloudDeviceId=<deviceId> to finish cloud relay setup.", tool)
		}
		return nil, fmt.Errorf(
			"%s needs a target. Call kvm_configure with action=set and the device url "+
				"(device LAN mode), or cloudUrl + cloudToken + cloudDeviceId together (cloud relay mode).", tool)
	}
	fingerprint := settings.Fingerprint()
	s.mu.Lock()
	if s.activeClient != nil && s.activeClient.fingerprint == fingerprint {
		client := s.activeClient.client
		s.mu.Unlock()
		return client, nil
	}
	entry := &clientEntry{
		fingerprint: fingerprint,
		client: device.NewClient(settings.URL, device.Options{
			AgentCookie:    settings.AgentCookie,
			AgentCsrfToken: settings.AgentCsrfToken,
			Password:       settings.Password,
			IceServersJSON: settings.IceServersJSON,
			CloudURL:       settings.CloudURL,
			CloudToken:     settings.CloudToken,
			CloudDeviceID:  settings.CloudDeviceID,
		}),
	}
	previous := s.activeClient
	s.activeClient = entry
	s.mu.Unlock()
	if previous != nil {
		go previous.client.Close()
	}
	return entry.client, nil
}

// resetClient drops the active client (settings changed / shutdown).
func (s *Server) resetClient() {
	s.CloseClient()
}

// persistCloudDeviceTarget records the picked cloud device target (id +
// resolved name) in the persisted token file so a restarted MCP keeps it.
// Only a file belonging to the same cloud session (same token, same cloudUrl
// when it states one) is touched — kvm_configure itself still never writes
// general settings to disk.
func (s *Server) persistCloudDeviceTarget(deviceID string, name string) {
	settings := s.Store.Get()
	persisted := config.ReadPersistedCloudToken()
	if persisted.Token == "" {
		return
	}
	if persisted.Token != settings.CloudToken {
		return
	}
	if persisted.CloudURL != "" && persisted.CloudURL != settings.CloudURL {
		return
	}
	if persisted.CloudDeviceID == deviceID && persisted.CloudDeviceName == name {
		return
	}
	persisted.CloudDeviceID = deviceID
	if name != "" {
		persisted.CloudDeviceName = name
	}
	if _, err := config.WritePersistedCloudToken(persisted); err == nil {
		logger.Info("Cloud device target recorded in the token file", map[string]any{"deviceId": deviceID, "name": name})
	}
}

// applyCloudDeviceName records the cloud device name next to the id and
// token, in memory and in the persisted token file. Best effort.
func (s *Server) applyCloudDeviceName(deviceID string, name string) {
	if name == "" {
		return
	}
	if s.Store.Get().CloudDeviceName == name {
		return
	}
	nameCopy := name
	_, _ = s.Store.Update(config.Update{CloudDeviceName: &nameCopy})
	s.persistCloudDeviceTarget(deviceID, name)
}

// refreshCloudDeviceName fetches the account's device list and records the
// configured device's cloud name (best effort).
func (s *Server) refreshCloudDeviceName(ctx context.Context, timeoutMs int64) (string, error) {
	settings := s.Store.Get()
	if settings.CloudURL == "" || settings.CloudToken == "" || settings.CloudDeviceID == "" {
		return "", nil
	}
	rows, err := cloudlogin.FetchCloudDevices(ctx, settings.CloudURL, settings.CloudToken, timeoutMs)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.DeviceID == settings.CloudDeviceID {
			if row.Name == "" {
				logger.Warn("Configured cloud device has no name in the account device list", map[string]any{"deviceId": settings.CloudDeviceID})
				return "", nil
			}
			s.applyCloudDeviceName(settings.CloudDeviceID, row.Name)
			return row.Name, nil
		}
	}
	logger.Warn("Configured cloud device is not in the account device list", map[string]any{"deviceId": settings.CloudDeviceID})
	return "", nil
}

type autoPickResult struct {
	Rows               []cloudlogin.CloudDeviceRow
	AutoPickedDeviceID string
}

// fetchDevicesAndAutoPickSingle fetches the account's devices and, when
// there is exactly one and no target is configured yet, selects it
// automatically. Best effort at the caller level.
func (s *Server) fetchDevicesAndAutoPickSingle(ctx context.Context, timeoutMs int64) (*autoPickResult, error) {
	settings := s.Store.Get()
	if settings.CloudURL == "" || settings.CloudToken == "" {
		return &autoPickResult{Rows: []cloudlogin.CloudDeviceRow{}}, nil
	}
	rows, err := cloudlogin.FetchCloudDevices(ctx, settings.CloudURL, settings.CloudToken, timeoutMs)
	if err != nil {
		return nil, err
	}
	if settings.CloudDeviceID == "" && len(rows) == 1 {
		id := rows[0].DeviceID
		name := rows[0].Name
		_, _ = s.Store.Update(config.Update{CloudDeviceID: &id, CloudDeviceName: &name})
		s.resetClient()
		logger.Info("Auto-selected the only cloud device on the account", map[string]any{"deviceId": id, "name": name})
		return &autoPickResult{Rows: rows, AutoPickedDeviceID: id}, nil
	}
	if settings.CloudDeviceID != "" {
		for _, row := range rows {
			if row.DeviceID == settings.CloudDeviceID {
				s.applyCloudDeviceName(settings.CloudDeviceID, row.Name)
				break
			}
		}
	}
	return &autoPickResult{Rows: rows}, nil
}

// applyManagedLoginToken is the background-login apply callback.
func (s *Server) applyManagedLoginToken(ctx context.Context, token, provider, kind string, persist bool) (*cloudlogin.ManagedDevicePickup, error) {
	capturedAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	tokenCopy := token
	capturedCopy := capturedAt
	_, err := s.Store.Update(config.Update{CloudToken: &tokenCopy, CloudTokenCapturedAt: &capturedCopy})
	if err != nil {
		return nil, err
	}
	s.resetClient()
	// A fresh token may complete the setup on its own: with exactly one
	// device on the account there is nothing to choose, so pick it (best
	// effort). The rows ride along so status() can tell a multi-device
	// account apart. The target and its cloud name are resolved BEFORE
	// persisting so the token file is written once with the complete set.
	var pickup *cloudlogin.ManagedDevicePickup
	result, fetchErr := s.fetchDevicesAndAutoPickSingle(ctx, 15_000)
	if fetchErr != nil {
		logger.Warn("Cloud device auto-selection after login failed", map[string]any{"error": fetchErr.Error()})
	} else {
		pickup = &cloudlogin.ManagedDevicePickup{
			Devices:            result.Rows,
			AutoPickedDeviceID: result.AutoPickedDeviceID,
		}
	}
	if persist {
		settings := s.Store.Get()
		if _, err := config.WritePersistedCloudToken(config.PersistedCloudToken{
			CloudURL:        settings.CloudURL,
			CloudDeviceID:   settings.CloudDeviceID,
			CloudDeviceName: settings.CloudDeviceName,
			Token:           token,
			Provider:        provider,
			CapturedAt:      capturedAt,
			Kind:            kind,
		}); err != nil {
			logger.Warn("Failed to persist cloud token", map[string]any{"error": err.Error()})
		}
	}
	logger.Info("Cloud login token applied", map[string]any{"provider": provider, "kind": kind, "persisted": persist})
	return pickup, nil
}

// StartupNameSelfHeal re-seeds the cloud device name when the startup seed
// (env/token file) lacks it: one background device-list call records it next
// to the id.
func (s *Server) StartupNameSelfHeal() {
	if config.ConnectionModeOf(s.Store.Get()) == config.ModeCloud && s.Store.Get().CloudDeviceName == "" {
		go func() {
			if _, err := s.refreshCloudDeviceName(context.Background(), 15_000); err != nil {
				logger.Warn("Startup cloud device name lookup failed", map[string]any{"error": err.Error()})
			}
		}()
	}
}

// ---- argument decoding helpers ----

func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(raw) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	return args, nil
}

func argString(args map[string]any, key string) (string, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", false
	}
	s, ok := value.(string)
	return s, ok
}

func argBool(args map[string]any, key string) (bool, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return false, false
	}
	b, ok := value.(bool)
	return b, ok
}

func argNumber(args map[string]any, key string) (float64, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return 0, false
	}
	f, ok := value.(float64)
	return f, ok
}

func argInt(args map[string]any, key string) (int64, bool) {
	f, ok := argNumber(args, key)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

func argMap(args map[string]any, key string) (map[string]any, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return nil, false
	}
	m, ok := value.(map[string]any)
	return m, ok
}

func argArray(args map[string]any, key string) ([]any, bool) {
	value, ok := args[key]
	if !ok || value == nil {
		return nil, false
	}
	a, ok := value.([]any)
	return a, ok
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func envTokenConfigured() bool {
	return os.Getenv("ARKKVM_CLOUD_TOKEN") != ""
}
