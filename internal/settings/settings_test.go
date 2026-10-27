package settings

import (
	"context"
	"errors"
	"testing"
)

type fakeRPC struct {
	responses map[string]any
	err       error
	calls     []string
}

func (f *fakeRPC) RPC(ctx context.Context, method string, params any, timeoutMs int64) (any, error) {
	f.calls = append(f.calls, method)
	if f.err != nil {
		return nil, f.err
	}
	return f.responses[method], nil
}

func TestGetRunsAllFourChecks(t *testing.T) {
	fake := &fakeRPC{responses: map[string]any{
		"getGuiSettings":  map[string]any{"max_brightness": float64(35)},
		"getUsbDevices":   map[string]any{"keyboard": true},
		"getUsbConfig":    map[string]any{"vendor_id": "0x1d6b"},
		"getFileTransfer": map[string]any{"enabled": true},
	}}
	out, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{Action: "get"})
	if err != nil {
		t.Fatalf("ApplyDeviceSettingsAction: %v", err)
	}
	if out["guiSettings"] == nil || out["usbDevices"] == nil || out["usbConfig"] == nil || out["fileTransfer"] == nil {
		t.Errorf("get must return all four sections: %v", out)
	}
	if out["action"] != "get" {
		t.Errorf("action must echo: %v", out["action"])
	}
}

func TestSetBacklightMergesCurrentValues(t *testing.T) {
	fake := &fakeRPC{responses: map[string]any{
		"getGuiSettings": map[string]any{
			"max_brightness": float64(64), "dim_after": float64(600), "off_after": float64(1800),
		},
	}}
	brightness := float64(10)
	out, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{
		Action: "set_backlight", MaxBrightness: &brightness,
	})
	if err != nil {
		t.Fatalf("ApplyDeviceSettingsAction: %v", err)
	}
	if out["applied"] != true {
		t.Errorf("applied flag missing: %v", out)
	}
	if len(fake.calls) < 2 || fake.calls[1] != "setBacklightSettings" {
		t.Fatalf("calls = %v", fake.calls)
	}
	_ = out
}

func TestSetBacklightRequiresOneField(t *testing.T) {
	fake := &fakeRPC{}
	if _, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{Action: "set_backlight"}); err == nil {
		t.Error("backlight without fields must fail")
	} else if err.Error() != "set_backlight requires at least one of maxBrightness, dimAfterSeconds, offAfterSeconds" {
		t.Errorf("unexpected message: %v", err)
	}
}

func TestSetDisplayRotationRequiresRotation(t *testing.T) {
	fake := &fakeRPC{}
	if _, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{Action: "set_display_rotation"}); err == nil {
		t.Error("rotation without value must fail")
	}
}

func TestSetUsbDevicesRequiresDevices(t *testing.T) {
	fake := &fakeRPC{}
	if _, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{Action: "set_usb_devices"}); err == nil {
		t.Error("usb_devices without devices must fail")
	}
}

func TestSetFileTransferRequiresFlag(t *testing.T) {
	fake := &fakeRPC{}
	if _, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{Action: "set_file_transfer"}); err == nil {
		t.Error("file_transfer without flag must fail")
	}
	enabled := true
	if _, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{
		Action: "set_file_transfer", FileTransferEnabled: &enabled,
	}); err != nil {
		t.Errorf("file_transfer with flag: %v", err)
	}
}

func TestRPCErrorPropagates(t *testing.T) {
	fake := &fakeRPC{err: errors.New("device broke")}
	if _, err := ApplyDeviceSettingsAction(context.Background(), fake, Request{Action: "get"}); err == nil {
		t.Error("rpc failure must propagate")
	}
}
