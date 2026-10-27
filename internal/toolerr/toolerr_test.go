package toolerr

import (
	"errors"
	"fmt"
	"testing"
)

func classifyMessage(msg string) StructuredErrorPayload {
	return Classify(errors.New(msg))
}

func TestClassifyDeviceAuth(t *testing.T) {
	payload := Classify(NewDeviceAuthError(401, ""))
	if payload.Code != CodeDeviceAuthRequired {
		t.Errorf("DeviceAuthError instance must classify as DEVICE_AUTH_REQUIRED, got %s", payload.Code)
	}
	if payload.Hint == "" {
		t.Error("device hint required")
	}
	for _, msg := range []string{
		"Unexpected server response: 401",
		"Unexpected server response: 403",
		"Device authentication failed: HTTP 401",
		"Device login failed: wrong password (HTTP 401)",
	} {
		if got := classifyMessage(msg); got.Code != CodeDeviceAuthRequired {
			t.Errorf("%q classified as %s", msg, got.Code)
		}
	}
	// Other handshake failures stay unclassified.
	for _, msg := range []string{
		"Unexpected server response: 502",
		"Device status failed: HTTP 500",
	} {
		if got := classifyMessage(msg); got.Code != CodeInternal {
			t.Errorf("%q classified as %s, want INTERNAL", msg, got.Code)
		}
	}
	// Cloud hint: fires only when a 401/403-shaped message also mentions
	// kvm_cloud_login.
	if got := classifyMessage("Device authentication failed: HTTP 401 (kvm_cloud_login action=login)"); got.Code != CodeDeviceAuthRequired || got.Hint == "" {
		t.Errorf("cloud auth message = %+v", got)
	}
}

func TestClassifyConnectionError(t *testing.T) {
	connErr := NewConnectionError(CodeConnectionLost, "WebRTC session torn down", 3)
	payload := Classify(connErr)
	if payload.Code != CodeConnectionLost || payload.Hint != connectionHints[CodeConnectionLost] {
		t.Errorf("ConnectionError payload = %+v", payload)
	}
}

func TestClassifyOrderedSpecifics(t *testing.T) {
	cases := []struct {
		msg  string
		code ToolErrorCode
	}{
		{"agentSlotOccupied: Agent session slot is already occupied", CodeAgentSlotOccupied},
		{"AgentSlotOccupied via casing", CodeAgentSlotOccupied},
		{"This tool needs a KVM device URL", CodeDeviceNotConfigured},
		{"RPC method is not exposed by local MCP: reboot", CodeRPCMethodNotExposed},
		{"RPC request timed out: ping (#12)", CodeConnectionLost},
		{"RPC connection timed out", CodeConnectionLost},
		{"Storage volume not mounted", CodeFTVolumeNotMounted},
		{"File already exists: ubuntu.iso", CodeFTFileExists},
		{"Storage upload incomplete: 100/200 bytes acknowledged", CodeUploadIncomplete},
		{"Storage upload source is not a regular file: /dev/null", CodeSourceFileInvalid},
		{"Storage upload refuses empty files", CodeSourceFileInvalid},
		{"Storage upload file exceeds the 17179869184-byte limit: /x", CodeSourceFileInvalid},
		{"set_display_rotation requires rotation", CodeInvalidArgument},
		{"set_backlight requires at least one of maxBrightness", CodeInvalidArgument},
		{"remotePath only applies when destination=fileTransfer", CodeInvalidArgument},
		{"kvm_storage_delete requires confirm=true; deletion cannot be undone.", CodeInvalidArgument},
		{"kvm_storage_delete destination=storage requires filename", CodeInvalidArgument},
		{"path only applies when destination=fileTransfer", CodeInvalidArgument},
		{"filename only applies when destination=storage", CodeInvalidArgument},
		{"cannot delete mounted storage image \"x.iso\"; unmount it first", CodeInvalidArgument},
		{"unsupported destination for kvm_storage_delete: nope", CodeInvalidArgument},
		{"File-transfer file path must not be the volume root", CodeInvalidArgument},
		{"Remote filename must not be empty", CodeInvalidArgument},
		{"-32603: Internal error (File already exists)", CodeFTFileExists},
		{"-32603: Internal error (USB service not initialized)", CodeRPCDeviceError},
		{"-32601: Method not found", CodeRPCDeviceError},
		{"totally unknown", CodeInternal},
	}
	for _, c := range cases {
		if got := classifyMessage(c.msg); got.Code != c.code {
			t.Errorf("classify(%q) = %s, want %s", c.msg, got.Code, c.code)
		}
	}
}

func TestClassifyDeviceErrorFields(t *testing.T) {
	// FT-matched causes still carry the device fields alongside the hint.
	payload := classifyMessage("-32603: Internal error (File already exists)")
	if payload.DeviceCode == nil || *payload.DeviceCode != -32603 {
		t.Errorf("deviceCode = %v", payload.DeviceCode)
	}
	if payload.DeviceData != "File already exists" {
		t.Errorf("deviceData = %q", payload.DeviceData)
	}
	payload = classifyMessage("-32603: Internal error (USB service not initialized)")
	if payload.Code != CodeRPCDeviceError || payload.DeviceData != "USB service not initialized" {
		t.Errorf("generic device error = %+v", payload)
	}
	payload = classifyMessage("-2: bare failure")
	if payload.DeviceCode == nil || *payload.DeviceCode != -2 || payload.DeviceData != "" {
		t.Errorf("bare device error fields = %+v", payload)
	}
	// A mid-message "-3: x" must not be treated as a device error envelope.
	if got := classifyMessage("step -3: x of y"); got.Code != CodeInternal {
		t.Errorf("mid-message code shape classified as %s", got.Code)
	}
}

func TestResultEnvelopeIsMirror(t *testing.T) {
	res := Result(map[string]any{"ok": true, "value": 7})
	if res.IsError {
		t.Error("success result must not be an error")
	}
	if res.ContentText != `{"ok":true,"value":7}` {
		t.Errorf("ContentText = %q", res.ContentText)
	}
}

func TestToolErrorPayload(t *testing.T) {
	code := int64(-32603)
	res := ToolError(CodeRPCDeviceError, "boom", ErrorOptions{Hint: "h", DeviceCode: &code, DeviceData: "d"})
	if !res.IsError {
		t.Error("ToolError must be an error result")
	}
	want := `{"ok":false,"code":"RPC_DEVICE_ERROR","message":"boom","hint":"h","deviceCode":-32603,"deviceData":"d"}`
	if res.ContentText != want {
		t.Errorf("ToolError JSON = %q, want %q", res.ContentText, want)
	}
}

func TestPlainToolError(t *testing.T) {
	res := PlainToolError(fmt.Errorf("simple failure"))
	if !res.IsError || res.ContentText != "simple failure" {
		t.Errorf("PlainToolError = %+v", res)
	}
}

func TestStructuredHandler(t *testing.T) {
	ok := StructuredHandler(func() (any, error) { return map[string]any{"fine": true}, nil })
	if ok.IsError {
		t.Error("success path must not error")
	}
	bad := StructuredHandler(func() (any, error) { return nil, errors.New("needs a KVM device URL") })
	if !bad.IsError {
		t.Error("failure path must be an error result")
	}
}
