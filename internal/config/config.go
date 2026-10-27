// Package config holds the KVM connection settings for this MCP process.
// Settings are in-memory only: they are seeded from the environment at
// startup and changed through the kvm_configure MCP tool. Nothing is written
// to disk (one exception: the cloud device target may be appended to the
// cloud token file), so a restarted server is back to the environment
// defaults.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ToolIDs lists every tool whose behavior depends on the connection
// configuration; device_capabilities reports it verbatim.
var ToolIDs = []string{
	"device_status",
	"kvm_rpc",
	"kvm_keyboard",
	"kvm_mouse",
	"kvm_wheel",
	"kvm_get_control",
	"kvm_request_control",
	"kvm_deny_control",
	"kvm_release_control",
	"kvm_ip_forward_usb",
	"kvm_ip_forward_host",
	"hdmi_screenshot",
	"hdmi_ocr",
	"hdmi_ocr_stream",
	"hdmi_frame_compare",
	"hdmi_wait_for_change",
	"hdmi_wait_for_stable",
	"hdmi_video_monitor",
	"kvm_key_sequence",
	"hdmi_wait_for_text",
	"kvm_health_snapshot",
	"kvm_virtual_media",
	"kvm_storage_upload",
	"kvm_storage_delete",
	"kvm_atx_power",
	"kvm_virtual_usb",
	"kvm_device_settings",
}

// Settings holds the KVM API settings. Empty string means unset.
type Settings struct {
	URL            string `json:"url,omitempty"`
	AgentCookie    string `json:"agentCookie,omitempty"`
	AgentCsrfToken string `json:"agentCsrfToken,omitempty"`
	Password       string `json:"password,omitempty"`
	IceServersJSON string `json:"iceServersJson,omitempty"`
	CloudURL       string `json:"cloudUrl,omitempty"`
	CloudToken     string `json:"cloudToken,omitempty"`
	CloudDeviceID  string `json:"cloudDeviceId,omitempty"`
	// CloudDeviceName is the cloud-side device name (not part of the
	// kvm_configure schema; resolved from the cloud device list and the
	// persisted token file).
	CloudDeviceName string `json:"cloudDeviceName,omitempty"`
	// CloudTokenCapturedAt is when the cloud token was captured (not part of
	// the kvm_configure schema; set by kvm_cloud_login).
	CloudTokenCapturedAt string `json:"cloudTokenCapturedAt,omitempty"`
}

// Fingerprint returns the identity used to decide whether the DeviceClient
// must be rebuilt (any settings change = disconnect and reconnect).
func (s Settings) Fingerprint() string {
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(data)
}

// Mode is the active transport: "cloud" relay or direct "device".
type Mode string

const (
	ModeCloud  Mode = "cloud"
	ModeDevice Mode = "device"
)

// ConnectionModeOf: cloud relay mode is explicit and complete-or-nothing —
// signaling through the cloud needs all three pieces (which cloud API, which
// session token, which device), so a partial set falls back to device mode
// and is rejected at update time with the missing pieces named.
func ConnectionModeOf(s Settings) Mode {
	if s.CloudURL != "" && s.CloudToken != "" && s.CloudDeviceID != "" {
		return ModeCloud
	}
	return ModeDevice
}

// Update is one kvm_configure action=set payload. Non-nil pointers change the
// field; Clear* flags drop fields.
type Update struct {
	URL                  *string
	AgentCookie          *string
	AgentCsrfToken       *string
	Password             *string
	IceServersJSON       *string
	CloudURL             *string
	CloudToken           *string
	CloudDeviceID        *string
	CloudDeviceName      *string
	CloudTokenCapturedAt *string

	ClearAgentCookie    bool
	ClearAgentCsrfToken bool
	ClearPassword       bool
	ClearIceServers     bool
	ClearCloudURL       bool
	ClearCloudToken     bool
	ClearCloudDeviceID  bool
}

// PublicConfig is the kvm_configure get/device_capabilities view. Secrets are
// only reported as *Configured booleans.
type PublicConfig struct {
	KvmAPI struct {
		URL                      string      `json:"url,omitempty"`
		AgentCookieConfigured    bool        `json:"agentCookieConfigured"`
		AgentCsrfTokenConfigured bool        `json:"agentCsrfTokenConfigured"`
		PasswordConfigured       bool        `json:"passwordConfigured"`
		IceServersJSON           string      `json:"iceServersJson,omitempty"`
		Cloud                    PublicCloud `json:"cloud"`
	} `json:"kvmApi"`
}

type PublicCloud struct {
	URL             string `json:"url,omitempty"`
	TokenConfigured bool   `json:"tokenConfigured"`
	TokenCapturedAt string `json:"tokenCapturedAt,omitempty"`
	DeviceID        string `json:"deviceId,omitempty"`
	DeviceName      string `json:"deviceName,omitempty"`
	Mode            Mode   `json:"mode"`
}

// Store holds the settings. All methods are safe for concurrent use; the MCP
// server serializes tool calls per session but background flows (cloud login
// apply) also touch it.
type Store struct {
	settings Settings
}

// New seeds a store from the environment (with token-file fallback).
func New() *Store {
	return &Store{settings: ConfiguredEnvironment()}
}

func (s *Store) Get() Settings { return s.settings }

func (s *Store) Mode() Mode { return ConnectionModeOf(s.settings) }

// Configured reports whether any connection target exists at all.
func (s *Store) Configured() bool {
	return s.settings.URL != "" || s.settings.CloudToken != "" || s.Mode() == ModeCloud
}

func (s *Store) PublicConfig() PublicConfig {
	var pc PublicConfig
	pc.KvmAPI.URL = s.settings.URL
	pc.KvmAPI.AgentCookieConfigured = s.settings.AgentCookie != ""
	pc.KvmAPI.AgentCsrfTokenConfigured = s.settings.AgentCsrfToken != ""
	pc.KvmAPI.PasswordConfigured = s.settings.Password != ""
	pc.KvmAPI.IceServersJSON = s.settings.IceServersJSON
	pc.KvmAPI.Cloud.URL = s.settings.CloudURL
	pc.KvmAPI.Cloud.TokenConfigured = s.settings.CloudToken != ""
	pc.KvmAPI.Cloud.TokenCapturedAt = s.settings.CloudTokenCapturedAt
	pc.KvmAPI.Cloud.DeviceID = s.settings.CloudDeviceID
	pc.KvmAPI.Cloud.DeviceName = s.settings.CloudDeviceName
	pc.KvmAPI.Cloud.Mode = s.Mode()
	return pc
}

// Update validates and applies one update, returning the new public config.
// Validation runs on the whole next state, after assignment.
func (s *Store) Update(update Update) (PublicConfig, error) {
	next := s.settings
	if update.URL != nil {
		next.URL = strings.TrimSpace(*update.URL)
	}
	if update.AgentCookie != nil {
		next.AgentCookie = *update.AgentCookie
	}
	if update.AgentCsrfToken != nil {
		next.AgentCsrfToken = *update.AgentCsrfToken
	}
	if update.Password != nil {
		next.Password = *update.Password
	}
	if update.IceServersJSON != nil {
		next.IceServersJSON = strings.TrimSpace(*update.IceServersJSON)
	}
	if update.CloudURL != nil {
		next.CloudURL = strings.TrimSpace(*update.CloudURL)
	}
	if update.CloudToken != nil {
		next.CloudToken = *update.CloudToken
		// A newly supplied token invalidates the previous capture time unless
		// the caller states otherwise.
		if update.CloudTokenCapturedAt == nil {
			next.CloudTokenCapturedAt = ""
		}
	}
	if update.CloudDeviceID != nil {
		// The device name belongs to a specific device: switching targets
		// invalidates it unless the caller supplies the new name in the same
		// update (re-setting the same id keeps the name).
		targetID := strings.TrimSpace(*update.CloudDeviceID)
		if targetID != s.settings.CloudDeviceID && update.CloudDeviceName == nil {
			next.CloudDeviceName = ""
		}
		next.CloudDeviceID = targetID
	}
	if update.CloudDeviceName != nil {
		next.CloudDeviceName = strings.TrimSpace(*update.CloudDeviceName)
	}
	if update.CloudTokenCapturedAt != nil {
		next.CloudTokenCapturedAt = strings.TrimSpace(*update.CloudTokenCapturedAt)
	}
	if update.ClearAgentCookie {
		next.AgentCookie = ""
	}
	if update.ClearAgentCsrfToken {
		next.AgentCsrfToken = ""
	}
	if update.ClearPassword {
		next.Password = ""
	}
	if update.ClearIceServers {
		next.IceServersJSON = ""
	}
	if update.ClearCloudURL {
		next.CloudURL = ""
		next.CloudDeviceName = ""
	}
	if update.ClearCloudToken {
		next.CloudToken = ""
		next.CloudTokenCapturedAt = ""
	}
	if update.ClearCloudDeviceID {
		next.CloudDeviceID = ""
		next.CloudDeviceName = ""
	}
	if err := ValidateURL(next.URL); err != nil {
		return PublicConfig{}, err
	}
	if err := ValidateURL(next.CloudURL); err != nil {
		return PublicConfig{}, err
	}
	if err := ValidateIceServers(next.IceServersJSON); err != nil {
		return PublicConfig{}, err
	}
	if err := validateCloudCompleteness(next); err != nil {
		return PublicConfig{}, err
	}
	s.settings = next
	return s.PublicConfig(), nil
}

// ResetToEnvironment re-seeds from env + token file (kvm_configure
// action=reset).
func (s *Store) ResetToEnvironment() PublicConfig {
	s.settings = ConfiguredEnvironment()
	return s.PublicConfig()
}

// ConfiguredEnvironment seeds settings from the environment with per-field
// fallback to the token file written by kvm_cloud_login persist:true. Env
// always wins.
func ConfiguredEnvironment() Settings {
	persisted := ReadPersistedCloudToken()
	envToken := os.Getenv("ARKKVM_CLOUD_TOKEN")
	s := Settings{
		URL:            os.Getenv("ARKKVM_URL"),
		AgentCookie:    firstEnv("ARKKVM_AGENT_COOKIE", "ARKKVM_COOKIE"),
		AgentCsrfToken: firstEnv("ARKKVM_AGENT_CSRF_TOKEN", "ARKKVM_CSRF_TOKEN"),
		Password:       firstEnv("ARKKVM_AGENT_PASSWORD", "ARKKVM_PASSWORD"),
		IceServersJSON: os.Getenv("ARKKVM_ICE_SERVERS"),
		CloudURL:       firstNonEmpty(os.Getenv("ARKKVM_CLOUD_URL"), persisted.CloudURL),
		// The capturedAt timestamp belongs to the file's token; an env token
		// has no known capture time.
		CloudToken:      firstNonEmpty(envToken, persisted.Token),
		CloudDeviceID:   firstNonEmpty(os.Getenv("ARKKVM_CLOUD_DEVICE_ID"), persisted.CloudDeviceID),
		CloudDeviceName: persisted.CloudDeviceName,
	}
	if envToken == "" {
		s.CloudTokenCapturedAt = persisted.CapturedAt
	}
	return s
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// ValidateURL requires an absolute http(s) URL.
func ValidateURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("KVM API URL must be a valid http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("KVM API URL must use http or https")
	}
	return nil
}

// ValidateIceServers checks that iceServersJson is a JSON array of ICE
// server objects.
func ValidateIceServers(iceServersJson string) error {
	if iceServersJson == "" {
		return nil
	}
	if _, err := ParseIceServers(iceServersJson); err != nil {
		return fmt.Errorf("ICE servers must be a JSON array: %s", err.Error())
	}
	return nil
}

// validateCloudCompleteness: cloudUrl is the root — token/deviceId without it
// is a misconfiguration. {cloudUrl, cloudDeviceId} without a token is a legal
// waiting state (kvm_cloud_login fills the token); {cloudUrl, cloudToken}
// without a deviceId is a legal "authenticated, target pending" state
// (kvm_cloud_login action=devices lists candidates). All three together
// switch the transport to cloud mode.
func validateCloudCompleteness(s Settings) error {
	if s.CloudURL == "" && s.CloudToken == "" && s.CloudDeviceID == "" {
		return nil
	}
	if s.CloudURL == "" {
		return fmt.Errorf("Cloud relay configuration is incomplete: cloudToken/cloudDeviceId also need cloudUrl. Set cloudUrl first (kvm_configure action=set cloudUrl=https://api.arkkvm.com for the global cloud, or https://api.arkkvm.cn for China).")
	}
	return nil
}

// IceServer mirrors the WebRTC ICE server configuration.
type IceServer struct {
	URLs       any    `json:"urls"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
}

// ParseIceServers parses the ARKKVM_ICE_SERVERS JSON (array required).
func ParseIceServers(value string) ([]IceServer, error) {
	if value == "" {
		return nil, nil
	}
	var parsed []IceServer
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil, fmt.Errorf("ARKKVM_ICE_SERVERS must be a JSON array")
	}
	return parsed, nil
}
