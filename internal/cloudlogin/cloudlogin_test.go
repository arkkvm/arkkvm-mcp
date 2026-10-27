package cloudlogin

import (
	"strings"
	"testing"
)

func TestNormalizeHTTPOrigin(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://api.arkkvm.com", "https://api.arkkvm.com"},
		{"  http://api.arkkvm.com/  ", "http://api.arkkvm.com"},
		{"https://api.arkkvm.com:8443", "https://api.arkkvm.com:8443"},
		{"ftp://api.arkkvm.com", ""},
		{"", ""},
		{"api.arkkvm.com", ""},
		{"https://user:pass@api.arkkvm.com", ""},
		{"https://ex%5Cample.com", ""},
		{"https://a b.com", ""},
	}
	for _, c := range cases {
		if got := NormalizeHTTPOrigin(c.in); got != c.want {
			t.Errorf("NormalizeHTTPOrigin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDeriveCloudAppOrigin(t *testing.T) {
	if got := DeriveCloudAppOrigin("https://api.arkkvm.com", ""); got != "https://app.arkkvm.com" {
		t.Errorf("canonical derivation = %q", got)
	}
	if got := DeriveCloudAppOrigin("https://api.example.io", ""); got != "https://app.example.io" {
		t.Errorf("api->app derivation = %q", got)
	}
	if got := DeriveCloudAppOrigin("https://api.arkkvm.cn", ""); got != "https://app.arkkvm.cn" {
		t.Errorf("China api->app derivation = %q", got)
	}
	if got := DeriveCloudAppOrigin("https://kvm.arkkvm.cn", ""); got != "https://app.arkkvm.cn" {
		t.Errorf("China non-api host keeps region: %q", got)
	}
	if got := DeriveCloudAppOrigin("https://kvm.arkkvm.com", ""); got != "https://app.arkkvm.com" {
		t.Errorf("global non-api host keeps region: %q", got)
	}
	if got := DeriveCloudAppOrigin("https://kvm.example.io", ""); got != "https://app.arkkvm.com" {
		t.Errorf("non-api host falls back to canonical: %q", got)
	}
	if got := DeriveCloudAppOrigin("https://api.arkkvm.com", "https://login.example.com"); got != "https://login.example.com" {
		t.Errorf("explicit appUrl wins: %q", got)
	}
	if got := DeriveCloudAppOrigin("not a url", ""); got != "" {
		t.Errorf("invalid api origin = %q", got)
	}
	// An invalid explicit appUrl must not fall back silently.
	if got := DeriveCloudAppOrigin("https://api.arkkvm.com", "also bad"); got != "" {
		t.Errorf("invalid explicit appUrl = %q", got)
	}
}

func TestDevicePickupHint(t *testing.T) {
	if DevicePickupHint(nil, true) != "" {
		t.Error("configured target needs no hint")
	}
	hint := DevicePickupHint(nil, false)
	if !strings.Contains(hint, "device list is unknown") {
		t.Errorf("unknown list hint = %q", hint)
	}
	two := &ManagedDevicePickup{Devices: []CloudDeviceRow{{DeviceID: "a"}, {DeviceID: "b"}}}
	hint = DevicePickupHint(two, false)
	if !strings.Contains(hint, "has 2 devices") {
		t.Errorf("multi-device hint = %q", hint)
	}
	one := &ManagedDevicePickup{Devices: []CloudDeviceRow{{DeviceID: "only-1"}}}
	hint = DevicePickupHint(one, false)
	if !strings.Contains(hint, "cloudDeviceId=only-1") {
		t.Errorf("single-device hint = %q", hint)
	}
	empty := &ManagedDevicePickup{Devices: []CloudDeviceRow{}}
	if got := DevicePickupHint(empty, false); got != "No devices on this account; pair one from the device web UI first." {
		t.Errorf("empty-account hint = %q", got)
	}
}

func TestManualTokenFallbackMentionsFlow(t *testing.T) {
	if !strings.Contains(ManualTokenFallback, "localStorage.getItem('token')") ||
		!strings.Contains(ManualTokenFallback, "cloudDeviceId=<deviceId>") {
		t.Error("manual fallback must carry the full manual path")
	}
	if !strings.Contains(ManualTokenFallback, "https://api.arkkvm.cn") {
		t.Error("manual fallback must mention the China cloud API")
	}
}
