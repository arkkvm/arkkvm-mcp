package protocol

import (
	"encoding/json"
	"testing"
)

func TestSanitizeVirtualUsbPath(t *testing.T) {
	if got, err := SanitizeVirtualUsbPath("  imgs//sub\\dir  "); err != nil || got != "imgs/sub/dir" {
		t.Errorf("SanitizeVirtualUsbPath = %q, %v", got, err)
	}
	if got, err := SanitizeVirtualUsbPath("/root"); err != nil || got != "root" {
		t.Errorf("leading slash must be stripped: %q, %v", got, err)
	}
	if _, err := SanitizeVirtualUsbPath(""); err == nil {
		t.Error("empty path must fail")
	}
	if _, err := SanitizeVirtualUsbPath("a\x00b"); err == nil {
		t.Error("NUL must fail")
	}
	if _, err := SanitizeVirtualUsbPath("a/../b"); err == nil {
		t.Error(`".." must fail`)
	}
}

func TestSanitizeFtFilePath(t *testing.T) {
	cases := []struct {
		in, want string
		fails    bool
	}{
		{"/docs/report.pdf", "/docs/report.pdf", false},
		{"docs\\report.pdf", "/docs/report.pdf", false},
		{"/docs//report.pdf", "/docs/report.pdf", false},
		{"", "", true},
		{"/", "", true},
		{"docs/", "", true},
		{"./report.pdf", "", true},
		{"docs/./report.pdf", "", true},
		{"../report.pdf", "", true},
		{"docs/../report.pdf", "", true},
		{"docs\x00/report.pdf", "", true},
	}
	for _, c := range cases {
		got, err := SanitizeFtFilePath(c.in)
		if c.fails && err == nil {
			t.Errorf("SanitizeFtFilePath(%q) must fail", c.in)
		}
		if !c.fails && (err != nil || got != c.want) {
			t.Errorf("SanitizeFtFilePath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestSanitizeRemoteFilename(t *testing.T) {
	if got, err := SanitizeRemoteFilename(" ubuntu.iso "); err != nil || got != "ubuntu.iso" {
		t.Errorf("SanitizeRemoteFilename = %q, %v", got, err)
	}
	if _, err := SanitizeRemoteFilename(""); err == nil {
		t.Error("empty must fail")
	}
	if _, err := SanitizeRemoteFilename("."); err == nil {
		t.Error(`"." must fail`)
	}
	if _, err := SanitizeRemoteFilename(".."); err == nil {
		t.Error(`".." must fail`)
	}
	if _, err := SanitizeRemoteFilename("a/b"); err == nil {
		t.Error(`"/" must fail`)
	}
	if _, err := SanitizeRemoteFilename("a\\b"); err == nil {
		t.Error(`backslash must fail`)
	}
	if _, err := SanitizeRemoteFilename("a\x07b"); err == nil {
		t.Error("control characters must fail")
	}
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := SanitizeRemoteFilename(string(long)); err == nil {
		t.Error("filename over 255 bytes must fail")
	}
	if _, err := SanitizeRemoteFilename(string(long[:255])); err != nil {
		t.Errorf("255-byte filename must pass: %v", err)
	}
}

func TestSanitizeHttpSourceUrl(t *testing.T) {
	if got, err := SanitizeHttpSourceUrl(" https://example.com/ubuntu.iso "); err != nil || got != "https://example.com/ubuntu.iso" {
		t.Errorf("valid URL rejected: %q, %v", got, err)
	}
	if _, err := SanitizeHttpSourceUrl("ftp://example.com/x.iso"); err == nil {
		t.Error("ftp scheme must fail with the scheme error")
	}
	if _, err := SanitizeHttpSourceUrl("notaurl"); err == nil {
		t.Error("relative garbage must fail")
	}
}

func TestValidateUploadStart(t *testing.T) {
	ok := json.RawMessage(`{"alreadyUploadedBytes": 1024, "dataChannel": "upload_abc"}`)
	start, err := ValidateStorageUploadStart(ok, 4096)
	if err != nil || start.AlreadyUploadedBytes != 1024 || start.DataChannel != "upload_abc" {
		t.Fatalf("ValidateStorageUploadStart = %+v, %v", start, err)
	}
	if _, err := ValidateStorageUploadStart(json.RawMessage(`{"dataChannel":"upload_abc"}`), 4096); err == nil {
		t.Error("missing offset must fail")
	}
	if _, err := ValidateStorageUploadStart(json.RawMessage(`{"alreadyUploadedBytes": 4097, "dataChannel":"upload_abc"}`), 4096); err == nil {
		t.Error("offset beyond size must fail")
	}
	if _, err := ValidateStorageUploadStart(json.RawMessage(`{"alreadyUploadedBytes": 1, "dataChannel":"ft_upload_abc"}`), 4096); err == nil {
		t.Error("wrong channel prefix must fail")
	}
	if _, err := ValidateFtUploadStart(json.RawMessage(`{"alreadyUploadedBytes": 1, "dataChannel":"ft_upload_abc"}`), 4096); err != nil {
		t.Errorf("ft prefix accepted for FT: %v", err)
	}
}

func TestSanitizeFtDirectoryPath(t *testing.T) {
	cases := []struct {
		in, want string
		fails    bool
	}{
		{"", "/", false},
		{"  ", "/", false},
		{"/", "/", false},
		{"isos", "/isos", false},
		{"/isos//sub/", "/isos/sub", false},
		{"a/./b", "", true},
		{"a/../b", "", true},
		{"a\x00b", "", true},
	}
	for _, c := range cases {
		got, err := SanitizeFtDirectoryPath(c.in)
		if c.fails && err == nil {
			t.Errorf("SanitizeFtDirectoryPath(%q) must fail", c.in)
		}
		if !c.fails && (err != nil || got != c.want) {
			t.Errorf("SanitizeFtDirectoryPath(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestParseUploadAck(t *testing.T) {
	ack := ParseUploadAck([]byte(`{"Size":4096,"AlreadyUploadedBytes":1024,"Extra":true}`))
	if ack == nil || ack.Size != 4096 || ack.AlreadyUploadedBytes != 1024 {
		t.Fatalf("ParseUploadAck = %+v", ack)
	}
	if ParseUploadAck([]byte(`not json`)) != nil {
		t.Error("garbage must return nil")
	}
	// Only the offset is checked for negativity; a negative size
	// still parses (device never sends one, and the ack is advisory).
	if ParseUploadAck([]byte(`{"Size":-1,"AlreadyUploadedBytes":0}`)) == nil {
		t.Error("negative size must still parse like Number() would")
	}
	if ParseUploadAck([]byte(`{"Size":1,"AlreadyUploadedBytes":-5}`)) != nil {
		t.Error("negative offset must return nil")
	}
}

func TestFormatRPCError(t *testing.T) {
	data := "File already exists"
	if got := FormatRPCError(RPCError{Code: -32603, Message: "Internal error", Data: &data}); got != "-32603: Internal error (File already exists)" {
		t.Errorf("FormatRPCError = %q", got)
	}
	if got := FormatRPCError(RPCError{Code: -32601, Message: "Method not found"}); got != "-32601: Method not found" {
		t.Errorf("FormatRPCError = %q", got)
	}
	blank := "   "
	if got := FormatRPCError(RPCError{Code: 1, Message: "m", Data: &blank}); got != "1: m" {
		t.Errorf("blank data must be dropped, got %q", got)
	}
}
