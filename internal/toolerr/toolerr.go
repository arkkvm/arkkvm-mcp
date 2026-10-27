// Package toolerr provides the shared tool-result plumbing: the JSON-mirror
// result envelope, the structured error format for tools that opt in, and the
// connection-plane error types. Failed calls return an isError result whose
// structuredContent carries a stable error code, an actionable hint, and the
// device's own JSON-RPC error fields, so clients can branch programmatically
// instead of parsing prose.
package toolerr

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ToolErrorCode values are stable identifiers clients branch on.
type ToolErrorCode string

const (
	CodeDeviceNotConfigured ToolErrorCode = "DEVICE_NOT_CONFIGURED"
	CodeDeviceAuthRequired  ToolErrorCode = "DEVICE_AUTH_REQUIRED"
	CodeConnectionNotReady  ToolErrorCode = "CONNECTION_NOT_READY"
	CodeConnectionLost      ToolErrorCode = "CONNECTION_LOST"
	CodeConnectionFailed    ToolErrorCode = "CONNECTION_FAILED"
	CodeAgentSlotOccupied   ToolErrorCode = "AGENT_SLOT_OCCUPIED"
	CodeRPCMethodNotExposed ToolErrorCode = "RPC_METHOD_NOT_EXPOSED"
	CodeRPCDeviceError      ToolErrorCode = "RPC_DEVICE_ERROR"
	CodeRPCTimeout          ToolErrorCode = "RPC_TIMEOUT"
	CodeFTVolumeNotMounted  ToolErrorCode = "FT_VOLUME_NOT_MOUNTED"
	CodeFTFileExists        ToolErrorCode = "FT_FILE_EXISTS"
	CodeUploadIncomplete    ToolErrorCode = "UPLOAD_INCOMPLETE"
	CodeSourceFileInvalid   ToolErrorCode = "SOURCE_FILE_INVALID"
	CodeInvalidArgument     ToolErrorCode = "INVALID_ARGUMENT"
	CodeInternal            ToolErrorCode = "INTERNAL"
)

// StructuredErrorPayload is the shape of structuredContent for failed calls.
type StructuredErrorPayload struct {
	OK         bool          `json:"ok"`
	Code       ToolErrorCode `json:"code"`
	Message    string        `json:"message"`
	Hint       string        `json:"hint,omitempty"`
	DeviceCode *int64        `json:"deviceCode,omitempty"`
	DeviceData string        `json:"deviceData,omitempty"`
}

// ToolResult is the transport-neutral result envelope; the server layer
// converts it to an MCP CallToolResult.
type ToolResult struct {
	// ContentText is the compact JSON mirror carried as text content.
	ContentText string
	// Structured mirrors ContentText as structuredContent.
	Structured any
	// IsError marks failed calls.
	IsError bool
}

// JSON renders the payload the way both content and structuredContent carry it.
func JSON(payload any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"ok":false,"code":"INTERNAL","message":"result serialization failed"}`
	}
	return string(data)
}

// Result wraps a successful payload in the standard JSON-mirror envelope.
func Result(payload any) *ToolResult {
	return &ToolResult{ContentText: JSON(payload), Structured: payload}
}

// ErrorOptions carry the optional structured error fields.
type ErrorOptions struct {
	Hint       string
	DeviceCode *int64
	DeviceData string
}

// ToolError builds an isError result with a stable error code.
func ToolError(code ToolErrorCode, message string, opts ErrorOptions) *ToolResult {
	payload := StructuredErrorPayload{OK: false, Code: code, Message: message}
	if opts.Hint != "" {
		payload.Hint = opts.Hint
	}
	payload.DeviceCode = opts.DeviceCode
	payload.DeviceData = opts.DeviceData
	return &ToolResult{ContentText: JSON(payload), Structured: payload, IsError: true}
}

// ConnectionError is the structured connection-plane error for Agent
// feedback. MCP does not auto-retry; the Agent decides whether to call again.
type ConnectionError struct {
	Code       ToolErrorCode
	Hint       string
	Generation int64 // 0 = unset
	Msg        string
}

func (e *ConnectionError) Error() string { return e.Msg }

// NewConnectionError builds a ConnectionError with the default hint for code.
func NewConnectionError(code ToolErrorCode, message string, generation int64) *ConnectionError {
	hint, ok := connectionHints[code]
	if !ok {
		hint = connectionHints[CodeConnectionFailed]
	}
	return &ConnectionError{Code: code, Hint: hint, Generation: generation, Msg: message}
}

var connectionHints = map[ToolErrorCode]string{
	CodeConnectionNotReady: "WebRTC session is not connected. Call a device tool again after the ArkKVM device is reachable; the adapter will ensureConnected on demand.",
	CodeConnectionLost:     "The WebRTC session was torn down (peer/signaling/datachannel death or RPC timeout). Retry the same tool when ready; the Agent decides timing.",
	CodeConnectionFailed:   "Failed to establish WebRTC to the ArkKVM device. Check ARKKVM_URL, network, and credentials via kvm_configure.",
}

// DeviceAuthError is raised when the device answers with HTTP 401/403 so
// callers can distinguish credential problems from other connection failures.
type DeviceAuthError struct {
	HTTPStatus int
	Msg        string
}

func (e *DeviceAuthError) Error() string { return e.Msg }

// NewDeviceAuthError builds a DeviceAuthError with the default message.
func NewDeviceAuthError(httpStatus int, message string) *DeviceAuthError {
	if message == "" {
		message = fmt.Sprintf("Device authentication failed: HTTP %d", httpStatus)
	}
	return &DeviceAuthError{HTTPStatus: httpStatus, Msg: message}
}

// Message returns err.Error() regardless of the dynamic type.
func message(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var (
	reDeviceAuth       = regexp.MustCompile(`Device (login|authentication) failed:.*HTTP 40[13]|Unexpected server response: 40[13]`)
	reCloudLogin       = regexp.MustCompile(`kvm_cloud_login`)
	reAgentSlot        = regexp.MustCompile(`(?i)agentSlotOccupied`)
	reNotConfigured    = regexp.MustCompile(`needs a KVM device URL`)
	reMethodNotExposed = regexp.MustCompile(`is not exposed by local MCP`)
	reRPCTimeout       = regexp.MustCompile(`RPC (request|connection) timed out`)
	reNotMounted       = regexp.MustCompile(`(?i)not mounted`)
	reAlreadyExists    = regexp.MustCompile(`(?i)already exists`)
	reUploadIncomplete = regexp.MustCompile(`upload incomplete`)
	reSourceInvalid    = regexp.MustCompile(`upload source is not a regular file|refuses empty files|upload file exceeds the .*-byte limit`)
	reInvalidArgument  = regexp.MustCompile(`requires (rotation|devices|usbConfig|fileTransferEnabled)|requires at least one of|remotePath only applies|kvm_storage_delete.*requires|only applies when destination=(storage|fileTransfer)|cannot delete mounted storage image|unsupported destination for kvm_storage_delete|File-transfer file path|Remote filename must`)
	reDeviceError      = regexp.MustCompile(`^(-?\d+):\s(.+?)(?:\s\((.+)\))?$`)
)

type deviceErrorFields struct {
	deviceCode int64
	deviceData string
	hasDevice  bool
}

// parseDeviceError parses the "-<code>: <message> (<data>)" shape that
// FormatRPCError produces for device JSON-RPC errors, so the code and the
// device's own cause travel as structured fields instead of prose.
func parseDeviceError(msg string) (deviceErrorFields, bool) {
	m := reDeviceError.FindStringSubmatch(msg)
	if m == nil {
		return deviceErrorFields{}, false
	}
	code, err := parseNumeric(m[1])
	if err != nil {
		return deviceErrorFields{}, false
	}
	fields := deviceErrorFields{deviceCode: code}
	if m[3] != "" {
		fields.deviceData = m[3]
	}
	fields.hasDevice = true
	return fields, true
}

// Classify maps an error to a stable error code payload. Order matters:
// specific device causes (surfaced by FormatRPCError as
// "-32603: Internal error (cause)") must be matched before the generic
// device-error fallback.
func Classify(err error) StructuredErrorPayload {
	msg := message(err)
	device, hasDevice := parseDeviceError(msg)

	var authErr *DeviceAuthError
	isAuthErr := AsDeviceAuthError(err, &authErr)

	var connErr *ConnectionError
	isConnErr := AsConnectionError(err, &connErr)

	if isAuthErr || reDeviceAuth.MatchString(msg) {
		payload := StructuredErrorPayload{OK: false, Code: CodeDeviceAuthRequired, Message: msg}
		if reCloudLogin.MatchString(msg) {
			payload.Hint = "The cloud session token is missing, expired, or revoked. Re-authenticate with kvm_cloud_login action=login, or set cloudToken via kvm_configure action=set."
		} else {
			payload.Hint = "The device enforces password authentication. Set the device password via kvm_configure action=set password=..., or supply agentCookie and agentCsrfToken. A wrong password also lands here."
		}
		return payload
	}
	if isConnErr {
		return StructuredErrorPayload{OK: false, Code: connErr.Code, Message: msg, Hint: connErr.Hint}
	}
	if reAgentSlot.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeAgentSlotOccupied, Message: msg,
			Hint: "Another agent already holds the device agent slot. Do not retry in a loop; stop the other agent session or wait for it to disconnect."}
	}
	if reNotConfigured.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeDeviceNotConfigured, Message: msg,
			Hint: "Call kvm_configure with action=set url=... first."}
	}
	if reMethodNotExposed.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeRPCMethodNotExposed, Message: msg,
			Hint: "kvm_rpc exposes read-only methods only; use a dedicated tool for writes or report unsupported."}
	}
	if reRPCTimeout.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeConnectionLost, Message: msg,
			Hint: "The WebRTC session was torn down after RPC timeout. Retry the same tool when ready; the Agent decides timing."}
	}
	if reNotMounted.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeFTVolumeNotMounted, Message: msg,
			Hint:       "Run kvm_virtual_usb with action=mount, or enable file transfer with kvm_device_settings action=set_file_transfer.",
			DeviceCode: deviceField(&device, hasDevice), DeviceData: device.deviceData}
	}
	if reAlreadyExists.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeFTFileExists, Message: msg,
			Hint:       "Pick another remoteFilename or delete the file first.",
			DeviceCode: deviceField(&device, hasDevice), DeviceData: device.deviceData}
	}
	if reUploadIncomplete.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeUploadIncomplete, Message: msg,
			Hint: "Rerun the same upload; it resumes from the acknowledged byte offset."}
	}
	if reSourceInvalid.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeSourceFileInvalid, Message: msg,
			Hint: "Check localPath on the controller machine."}
	}
	if reInvalidArgument.MatchString(msg) {
		return StructuredErrorPayload{OK: false, Code: CodeInvalidArgument, Message: msg}
	}
	if hasDevice {
		return StructuredErrorPayload{OK: false, Code: CodeRPCDeviceError, Message: msg,
			DeviceCode: &device.deviceCode, DeviceData: device.deviceData}
	}
	return StructuredErrorPayload{OK: false, Code: CodeInternal, Message: msg}
}

func deviceField(d *deviceErrorFields, ok bool) *int64 {
	if !ok {
		return nil
	}
	code := d.deviceCode
	return &code
}

// ClassifyToolError classifies and wraps into the isError ToolResult.
func ClassifyToolError(err error) *ToolResult {
	payload := Classify(err)
	return &ToolResult{ContentText: JSON(payload), Structured: payload, IsError: true}
}

// StructuredHandler wraps a handler whose success payload is already built so
// thrown errors become structured isError results.
func StructuredHandler(handler func() (any, error)) *ToolResult {
	payload, err := handler()
	if err != nil {
		return ClassifyToolError(err)
	}
	return Result(payload)
}

// PlainToolError renders an error the way the MCP SDK does for tools that do
// not opt into structured errors: the bare message becomes the result text.
func PlainToolError(err error) *ToolResult {
	return &ToolResult{ContentText: message(err), Structured: nil, IsError: true}
}

// ---- errors.As helpers kept local so this package has no dependency
// direction issues -----------------------------------------------
