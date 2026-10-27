package config

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// isolateEnv blanks every ARKKVM_* variable and points HOME at a scratch
// dir so tests never see the developer's real cloud-token.json or env.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ARKKVM_URL", "ARKKVM_AGENT_COOKIE", "ARKKVM_COOKIE",
		"ARKKVM_AGENT_CSRF_TOKEN", "ARKKVM_CSRF_TOKEN",
		"ARKKVM_AGENT_PASSWORD", "ARKKVM_PASSWORD",
		"ARKKVM_ICE_SERVERS", "ARKKVM_CLOUD_URL", "ARKKVM_CLOUD_TOKEN",
		"ARKKVM_CLOUD_DEVICE_ID",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

func TestConnectionModeOf(t *testing.T) {
	cases := []struct {
		s    Settings
		want Mode
	}{
		{Settings{}, ModeDevice},
		{Settings{CloudURL: "https://api", CloudToken: "t"}, ModeDevice},
		{Settings{CloudURL: "https://api", CloudDeviceID: "d"}, ModeDevice},
		{Settings{CloudURL: "https://api", CloudToken: "t", CloudDeviceID: "d"}, ModeCloud},
	}
	for i, c := range cases {
		if got := ConnectionModeOf(c.s); got != c.want {
			t.Errorf("case %d: mode = %s, want %s", i, got, c.want)
		}
	}
}

func TestStoreUpdateSemantics(t *testing.T) {
	isolateEnv(t)
	store := New()

	str := func(s string) *string { return &s }

	if _, err := store.Update(Update{URL: str("ftp://bad")}); err == nil {
		t.Error("bad scheme must fail")
	}
	if _, err := store.Update(Update{URL: str("not a url")}); err == nil {
		t.Error("garbage URL must fail")
	}
	if _, err := store.Update(Update{IceServersJSON: str("{not array")}); err == nil {
		t.Error("bad ICE JSON must fail")
	}
	// Cloud completeness: token/deviceId need cloudUrl.
	if _, err := store.Update(Update{CloudToken: str("tok")}); err == nil {
		t.Error("token without cloudUrl must fail")
	} else if !strings.Contains(err.Error(), "api.arkkvm.cn") {
		t.Errorf("cloud URL error must mention the China cloud: %v", err)
	}
	// Legal intermediate states.
	if _, err := store.Update(Update{CloudURL: str("https://api.example.com")}); err != nil {
		t.Errorf("cloudUrl alone must be legal: %v", err)
	}
	if _, err := store.Update(Update{CloudDeviceID: str("dev-1")}); err != nil {
		t.Errorf("cloudUrl+deviceId without token must be legal: %v", err)
	}
	if store.Mode() != ModeDevice {
		t.Error("missing token must stay device mode")
	}
	if _, err := store.Update(Update{CloudToken: str("tok"), CloudTokenCapturedAt: str("2026-01-01T00:00:00Z")}); err != nil {
		t.Fatalf("full cloud config rejected: %v", err)
	}
	if store.Mode() != ModeCloud {
		t.Error("all three pieces must switch to cloud mode")
	}
	pc := store.PublicConfig()
	if !pc.KvmAPI.Cloud.TokenConfigured || pc.KvmAPI.Cloud.DeviceID != "dev-1" {
		t.Errorf("public config = %+v", pc.KvmAPI.Cloud)
	}

	// Changing the device id clears the name unless a new name is supplied.
	if _, err := store.Update(Update{CloudDeviceName: str("kvm-office")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(Update{CloudDeviceID: str("dev-2")}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CloudDeviceName != "" {
		t.Error("switching device target must clear the name")
	}
	if _, err := store.Update(Update{CloudDeviceID: str("dev-3"), CloudDeviceName: str("kvm-lab")}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CloudDeviceName != "kvm-lab" {
		t.Error("explicit name in the same update must be kept")
	}
	// Re-setting the same id keeps the name.
	if _, err := store.Update(Update{CloudDeviceID: str("dev-3")}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CloudDeviceName != "kvm-lab" {
		t.Error("same id must keep the name")
	}
	// Setting a new token clears the capturedAt unless stated.
	if _, err := store.Update(Update{CloudToken: str("tok2")}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CloudTokenCapturedAt != "" {
		t.Error("new token must clear capturedAt")
	}
	// Clearing only the URL while token/deviceId remain is incomplete and
	// fails validation (settings stay unchanged).
	if _, err := store.Update(Update{ClearCloudURL: true}); err == nil {
		t.Error("clearCloudUrl with token+deviceId still set must fail completeness")
	}
	// Clearing everything is legal.
	if _, err := store.Update(Update{ClearCloudURL: true, ClearCloudToken: true, ClearCloudDeviceID: true}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CloudURL != "" || store.Get().CloudToken != "" || store.Get().CloudDeviceID != "" || store.Get().CloudDeviceName != "" {
		t.Errorf("clear-all must empty the cloud config: %+v", store.Get())
	}
}

func TestStoreResetToEnvironment(t *testing.T) {
	isolateEnv(t)
	t.Setenv("ARKKVM_URL", "https://device.example.com")
	t.Setenv("ARKKVM_AGENT_PASSWORD", "env-secret")
	t.Setenv("ARKKVM_PASSWORD", "plain-secret")
	t.Setenv("ARKKVM_AGENT_COOKIE", "")
	t.Setenv("ARKKVM_COOKIE", "")
	// Env token beats file token; HOME is isolated so no real token file leaks in.
	t.Setenv("HOME", t.TempDir())
	store := New()
	if store.Get().Password != "env-secret" {
		t.Errorf("ARKKVM_AGENT_PASSWORD must win over ARKKVM_PASSWORD, got %q", store.Get().Password)
	}
	if store.Get().URL != "https://device.example.com" {
		t.Errorf("URL not seeded: %q", store.Get().URL)
	}
	if !store.PublicConfig().KvmAPI.PasswordConfigured {
		t.Error("passwordConfigured must be true")
	}
	// The public config type has no password field at all; assert the secret
	// value never leaks through its JSON encoding either.
	publicJSON, _ := json.Marshal(store.PublicConfig())
	if bytes.Contains(publicJSON, []byte("env-secret")) {
		t.Error("public config must never expose the password value")
	}

	// Update then reset returns to the environment.
	str := "runtime-override"
	if _, err := store.Update(Update{Password: &str}); err != nil {
		t.Fatal(err)
	}
	if store.Get().Password != "runtime-override" {
		t.Error("runtime password must apply")
	}
	store.ResetToEnvironment()
	if store.Get().Password != "env-secret" {
		t.Error("reset must re-seed from the environment")
	}
}

func TestTokenFileRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if ReadPersistedCloudToken().Token != "" {
		t.Error("missing file must read as empty")
	}
	if ClearPersistedCloudToken() {
		t.Error("clearing a missing file must report false")
	}
	_, err := WritePersistedCloudToken(PersistedCloudToken{
		CloudURL:        "https://api.example.com",
		Token:           "tok",
		CloudDeviceID:   "dev-9",
		CloudDeviceName: "kvm",
		Kind:            TokenKindMCP,
		CapturedAt:      "2026-01-02T03:04:05Z",
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	path := CloudTokenFilePath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
	persisted := ReadPersistedCloudToken()
	if persisted.Token != "tok" || persisted.CloudDeviceID != "dev-9" || persisted.CloudDeviceName != "kvm" {
		t.Errorf("round trip mismatch: %+v", persisted)
	}
	// A corrupt file reads as empty, not a crash.
	_ = os.WriteFile(path, []byte("{corrupt"), 0o600)
	if ReadPersistedCloudToken().Token != "" {
		t.Error("corrupt file must read as empty")
	}
	// A file without a token reads as empty.
	_ = os.WriteFile(path, []byte(`{"cloudUrl":"https://x"}`), 0o600)
	if ReadPersistedCloudToken().Token != "" {
		t.Error("token-less file must read as empty")
	}
	if !ClearPersistedCloudToken() {
		t.Error("clearing an existing file must report true")
	}
	if ClearPersistedCloudToken() {
		t.Error("second clear must report false")
	}
}

func TestEnvironmentFallsBackToTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := WritePersistedCloudToken(PersistedCloudToken{
		CloudURL:   "https://api.file.example.com",
		Token:      "file-token",
		CapturedAt: "2026-02-03T04:05:06Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARKKVM_CLOUD_URL", "")
	t.Setenv("ARKKVM_CLOUD_TOKEN", "")
	t.Setenv("ARKKVM_CLOUD_DEVICE_ID", "")
	s := ConfiguredEnvironment()
	if s.CloudURL != "https://api.file.example.com" || s.CloudToken != "file-token" {
		t.Errorf("file fallback failed: %+v", s)
	}
	if s.CloudTokenCapturedAt != "2026-02-03T04:05:06Z" {
		t.Errorf("file capturedAt must seed: %q", s.CloudTokenCapturedAt)
	}
	// env token has no capture time and beats the file value.
	t.Setenv("ARKKVM_CLOUD_TOKEN", "env-token")
	s = ConfiguredEnvironment()
	if s.CloudToken != "env-token" || s.CloudTokenCapturedAt != "" {
		t.Errorf("env token must win and carry no capturedAt: %+v", s)
	}
}
