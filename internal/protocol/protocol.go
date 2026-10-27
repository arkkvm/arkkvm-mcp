// Package protocol holds the typed protocol surface shared by the MCP tools
// and the device client for the ArkKVM virtual-media, storage-upload, and
// ATX JSON-RPC methods. Shapes mirror the device-side registration in
// arkkvm-app jsonrpc.rs.
package protocol

import (
	"encoding/json"
	"fmt"
	neturl "net/url"
	"strconv"
	"strings"
)

// Virtual media ---------------------------------------------------------

type VirtualMediaMode string

const (
	MediaModeCDROM VirtualMediaMode = "CDROM"
	MediaModeDisk  VirtualMediaMode = "DISK"
)

// VirtualMediaModes is the accepted mode list, in device order.
var VirtualMediaModes = []VirtualMediaMode{MediaModeCDROM, MediaModeDisk}

type VirtualMediaSource string

const (
	SourceWebRTC  VirtualMediaSource = "WebRTC"
	SourceHTTP    VirtualMediaSource = "HTTP"
	SourceStorage VirtualMediaSource = "Storage"
)

type VirtualMediaState struct {
	Source   VirtualMediaSource `json:"source"`
	Mode     VirtualMediaMode   `json:"mode"`
	Filename string             `json:"filename,omitempty"`
	URL      string             `json:"url,omitempty"`
	Size     int64              `json:"size"`
}

type VirtualMediaItem struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	IconKey string `json:"iconKey"`
}

// Upload start ----------------------------------------------------------

type StorageUploadStartResponse struct {
	AlreadyUploadedBytes int64
	DataChannel          string
}

// ATX power -------------------------------------------------------------

type AtxPowerAction string

const (
	AtxPowerShort AtxPowerAction = "power-short"
	AtxPowerLong  AtxPowerAction = "power-long"
	AtxPowerReset AtxPowerAction = "reset"
)

// AtxPowerActions is the accepted action list, in tool order.
var AtxPowerActions = []AtxPowerAction{AtxPowerShort, AtxPowerLong, AtxPowerReset}

// Virtual USB -----------------------------------------------------------

type VirtualUsbTarget string

const (
	VirtualUsbKvm       VirtualUsbTarget = "Kvm"
	VirtualUsbRemoteUsb VirtualUsbTarget = "RemoteUsb"
)

// VirtualUsbTargets is the accepted target list.
var VirtualUsbTargets = []VirtualUsbTarget{VirtualUsbKvm, VirtualUsbRemoteUsb}

type VirtualUsbFileRow struct {
	Name     string `json:"name"`
	IsDir    bool   `json:"is_dir,omitempty"`
	Size     *int64 `json:"size,omitempty"`
	Accessed string `json:"accessed,omitempty"`
	Created  string `json:"created,omitempty"`
	Modified string `json:"modified,omitempty"`
}

// Path and filename sanitizers ------------------------------------------

// SanitizeVirtualUsbPath validates a path inside the device file-transfer
// filesystem. The device rejects absolute paths and `..` components on its
// side; the MCP mirrors that up front so callers get an immediate local error.
func SanitizeVirtualUsbPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("Virtual USB path must not be empty")
	}
	if strings.Contains(trimmed, "\x00") {
		return "", fmt.Errorf("Virtual USB path must not contain NUL")
	}
	if strings.Contains(trimmed, "..") {
		return "", fmt.Errorf(`Virtual USB path must not contain ".."`)
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	for strings.Contains(normalized, "//") {
		normalized = strings.ReplaceAll(normalized, "//", "/")
	}
	return strings.TrimPrefix(normalized, "/"), nil
}

// SanitizeFtFilePath validates a single file path on the device file-transfer
// volume and returns it with a leading slash, matching the ftDelFile RPC.
// Deleting the volume root, directories, or paths with empty/"." segments is
// rejected locally so a destructive call fails before reaching the device.
func SanitizeFtFilePath(path string) (string, error) {
	safePath, err := SanitizeVirtualUsbPath(path)
	if err != nil {
		return "", err
	}
	if safePath == "" {
		return "", fmt.Errorf("File-transfer file path must not be the volume root")
	}
	for _, segment := range strings.Split(safePath, "/") {
		if segment == "" {
			return "", fmt.Errorf("File-transfer file path must not end with a slash")
		}
		if segment == "." {
			return "", fmt.Errorf(`File-transfer file path must not contain "." segments`)
		}
	}
	return "/" + safePath, nil
}

// MaxFilenameBytes is the device-side filename limit.
const MaxFilenameBytes = 255

// SanitizeRemoteFilename validates a filename that will be sent to the
// device's images folder or FT filesystem. The device additionally applies
// basename/`..` rejection on its side; the MCP rejects separators up front so
// the AI caller gets an immediate, local error instead of a deferred
// device-side failure.
func SanitizeRemoteFilename(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("Remote filename must not be empty")
	}
	if len(trimmed) > MaxFilenameBytes {
		return "", fmt.Errorf("Remote filename exceeds %d bytes", MaxFilenameBytes)
	}
	if trimmed == "." || trimmed == ".." {
		return "", fmt.Errorf(`Remote filename must not be "." or ".."`)
	}
	if strings.ContainsAny(trimmed, "/\\\x00") {
		return "", fmt.Errorf("Remote filename must not contain \"/\" or \"\\\\\": %s", jsonString(trimmed))
	}
	for _, r := range trimmed {
		if r <= 0x1f || r == 0x7f {
			return "", fmt.Errorf("Remote filename must not contain control characters")
		}
	}
	return trimmed, nil
}

// SanitizeHttpSourceUrl validates an http(s) URL used for mountWithHTTP and
// returns the normalized URL string.
func SanitizeHttpSourceUrl(url string) (string, error) {
	trimmed := strings.TrimSpace(url)
	u, err := neturl.Parse(trimmed)
	// Mirror new URL(): only absolute URLs with a host parse at all.
	if err != nil || !u.IsAbs() || u.Host == "" {
		return "", fmt.Errorf("Virtual media URL is not valid: %s", jsonString(trimmed))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("Virtual media URL must use http or https; other schemes are rejected")
	}
	return trimmed, nil
}

// Upload start validation ------------------------------------------------

// ValidateStorageUploadStart validates the startStorageFileUpload response
// before an upload channel is created.
func ValidateStorageUploadStart(response json.RawMessage, expectedSize int64) (StorageUploadStartResponse, error) {
	return validateUploadStart(response, expectedSize, "upload_", "storage upload")
}

// ValidateFtUploadStart validates the startFtFsUpload response. The device
// hands out a dedicated ft_upload_<uuid> DataChannel, distinct from the
// storage upload_<uuid>.
func ValidateFtUploadStart(response json.RawMessage, expectedSize int64) (StorageUploadStartResponse, error) {
	return validateUploadStart(response, expectedSize, "ft_upload_", "file-transfer upload")
}

func validateUploadStart(response json.RawMessage, expectedSize int64, channelPrefix, kind string) (StorageUploadStartResponse, error) {
	var value struct {
		AlreadyUploadedBytes json.Number `json:"alreadyUploadedBytes"`
		DataChannel          string      `json:"dataChannel"`
	}
	if err := json.Unmarshal(response, &value); err != nil {
		return StorageUploadStartResponse{}, fmt.Errorf("Device returned an invalid %s start response", kind)
	}
	if value.DataChannel == "" || !strings.HasPrefix(value.DataChannel, channelPrefix) {
		return StorageUploadStartResponse{}, fmt.Errorf("Device returned an invalid %s data channel label", kind)
	}
	already, err := value.AlreadyUploadedBytes.Int64()
	if err != nil {
		// Mirror Number(): a string digit form is accepted, anything else is not.
		f, ferr := value.AlreadyUploadedBytes.Float64()
		if ferr != nil {
			return StorageUploadStartResponse{}, fmt.Errorf(
				"Device returned invalid resume offset %s for a %d-byte upload",
				value.AlreadyUploadedBytes.String(), expectedSize)
		}
		already = int64(f)
	}
	if already < 0 || already > expectedSize {
		return StorageUploadStartResponse{}, fmt.Errorf(
			"Device returned invalid resume offset %s for a %d-byte upload",
			value.AlreadyUploadedBytes.String(), expectedSize)
	}
	return StorageUploadStartResponse{AlreadyUploadedBytes: already, DataChannel: value.DataChannel}, nil
}

// FT directory paths -----------------------------------------------------

// SanitizeFtDirectoryPath validates a destination directory on the device
// file-transfer volume, used by startFtFsUpload. The device joins `path` +
// "/" + `name` and collapses duplicate slashes, and the frontend sends "/"
// for the volume root, so the MCP normalizes to a leading-slash directory
// ("/" or "/sub/dir") and rejects traversal up front for an immediate local
// error.
func SanitizeFtDirectoryPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "/", nil
	}
	if strings.Contains(trimmed, "\x00") {
		return "", fmt.Errorf("File-transfer path must not contain NUL")
	}
	if strings.Contains(trimmed, "..") {
		return "", fmt.Errorf(`File-transfer path must not contain ".."`)
	}
	for _, r := range trimmed {
		if r <= 0x1f || r == 0x7f {
			return "", fmt.Errorf("File-transfer path must not contain control characters")
		}
	}
	relative := strings.ReplaceAll(trimmed, "\\", "/")
	for strings.Contains(relative, "//") {
		relative = strings.ReplaceAll(relative, "//", "/")
	}
	relative = strings.Trim(relative, "/")
	if relative == "" {
		return "/", nil
	}
	for _, seg := range strings.Split(relative, "/") {
		if seg == "." {
			return "", fmt.Errorf(`File-transfer path must not contain "." segments`)
		}
	}
	return "/" + relative, nil
}

// Upload acknowledgement --------------------------------------------------

// UploadAck is one {"Size":N,"AlreadyUploadedBytes":M} progress message from
// the upload channel.
type UploadAck struct {
	Size                 int64
	AlreadyUploadedBytes int64
}

// ParseUploadAck parses one progress message. Anything that is not a JSON
// object with finite numeric fields returns nil.
func ParseUploadAck(data []byte) *UploadAck {
	var parsed struct {
		Size                 json.Number `json:"Size"`
		AlreadyUploadedBytes json.Number `json:"AlreadyUploadedBytes"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	size, err1 := strconv.ParseFloat(parsed.Size.String(), 64)
	already, err2 := strconv.ParseFloat(parsed.AlreadyUploadedBytes.String(), 64)
	if err1 != nil || err2 != nil || already < 0 {
		return nil
	}
	return &UploadAck{Size: int64(size), AlreadyUploadedBytes: int64(already)}
}

// RPC error formatting ----------------------------------------------------

// RPCError is a device JSON-RPC error object.
type RPCError struct {
	Code    int     `json:"code"`
	Message string  `json:"message"`
	Data    *string `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return FormatRPCError(*e) }

// FormatRPCError formats a JSON-RPC error object for an Error message. The
// device puts the human-readable cause (its anyhow chain) into `data` — the
// web frontend reads `e.data || e.message` — so the MCP must surface it too,
// otherwise handler failures like "File already exists" collapse into a bare
// "Internal error".
func FormatRPCError(e RPCError) string {
	data := ""
	if e.Data != nil {
		data = strings.TrimSpace(*e.Data)
	}
	if data != "" {
		return fmt.Sprintf("%d: %s (%s)", e.Code, e.Message, data)
	}
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

// helpers -----------------------------------------------------------------

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return strconv.Quote(s)
	}
	return string(b)
}
