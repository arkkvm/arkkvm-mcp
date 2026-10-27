package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Token kind values for PersistedCloudToken.Kind.
const (
	TokenKindMCP     = "mcp"     // dedicated grant-code token
	TokenKindSession = "session" // legacy web session capture
)

// PersistedCloudToken is the 0600 token file under
// ~/.config/arkkvm-mcp/cloud-token.json — the only disk state this MCP owns.
type PersistedCloudToken struct {
	CloudURL      string `json:"cloudUrl,omitempty"`
	CloudDeviceID string `json:"cloudDeviceId,omitempty"`
	// CloudDeviceName is the cloud-side device name for CloudDeviceID,
	// resolved from the account device list (best effort).
	CloudDeviceName string `json:"cloudDeviceName,omitempty"`
	Token           string `json:"token"`
	Provider        string `json:"provider,omitempty"`
	CapturedAt      string `json:"capturedAt,omitempty"`
	Kind            string `json:"kind,omitempty"`
}

// CloudTokenFilePath returns the single persistence path.
func CloudTokenFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return filepath.Join(home, ".config", "arkkvm-mcp", "cloud-token.json")
}

// ReadPersistedCloudToken returns the persisted token, or the zero value when
// the file is missing, unreadable, corrupt, or carries no token string.
func ReadPersistedCloudToken() PersistedCloudToken {
	file := CloudTokenFilePath()
	data, err := os.ReadFile(file)
	if err != nil {
		return PersistedCloudToken{}
	}
	var parsed PersistedCloudToken
	if err := json.Unmarshal(data, &parsed); err != nil {
		return PersistedCloudToken{}
	}
	if parsed.Token == "" {
		return PersistedCloudToken{}
	}
	return parsed
}

// WritePersistedCloudToken writes the token file as indented JSON with 0600
// permissions and returns the path.
func WritePersistedCloudToken(data PersistedCloudToken) (string, error) {
	file := CloudTokenFilePath()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	buffer, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	buffer = append(buffer, '\n')
	// Write with the restrictive mode first, then enforce it in case the file
	// already existed with wider permissions.
	if err := os.WriteFile(file, buffer, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return "", err
	}
	return file, nil
}

// ClearPersistedCloudToken removes the token file, reporting whether it
// existed.
func ClearPersistedCloudToken() bool {
	file := CloudTokenFilePath()
	if _, err := os.Stat(file); err != nil {
		return false
	}
	return os.Remove(file) == nil
}
