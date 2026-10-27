// Package settings implements the kvm_device_settings actions: read the
// frontend Device settings page state, or apply one setting and read its
// state back. set_backlight / set_usb_devices merge omitted fields with the
// current values because the device RPCs replace the whole setting.
package settings

import (
	"context"
	"fmt"
	"regexp"
)

// RPCClient is the minimal DeviceClient surface used here, so tests can
// substitute a fake.
type RPCClient interface {
	RPC(ctx context.Context, method string, params any, timeoutMs int64) (any, error)
}

// NetworkAdapter mirrors the USB network adapter block.
type NetworkAdapter struct {
	Enable      bool      `json:"enable"`
	AdapterType string    `json:"adapter_type,omitempty"`
	L3Config    *L3Config `json:"l3_config,omitempty"`
}

// L3Config is the board-side USB netdev L3 configuration.
type L3Config struct {
	IPAddress  string   `json:"ip_address"`
	SubnetMask string   `json:"subnet_mask"`
	Gateway    string   `json:"gateway,omitempty"`
	DNSServers []string `json:"dns_servers,omitempty"`
}

// UsbDevices mirrors the UsbDevicesState block.
type UsbDevices struct {
	Keyboard       bool            `json:"keyboard"`
	AbsoluteMouse  bool            `json:"absolute_mouse"`
	RelativeMouse  bool            `json:"relative_mouse"`
	MassStorage    bool            `json:"mass_storage"`
	Microphone     bool            `json:"microphone"`
	NetworkAdapter *NetworkAdapter `json:"network_adapter,omitempty"`
}

// UsbConfig mirrors the USB identity strings.
type UsbConfig struct {
	VendorID     string `json:"vendor_id"`
	ProductID    string `json:"product_id"`
	SerialNumber string `json:"serial_number"`
	Manufacturer string `json:"manufacturer"`
	Product      string `json:"product"`
}

// Request is one kvm_device_settings call.
type Request struct {
	Action              string      `json:"action"`
	MaxBrightness       *float64    `json:"maxBrightness,omitempty"`
	DimAfterSeconds     *float64    `json:"dimAfterSeconds,omitempty"`
	OffAfterSeconds     *float64    `json:"offAfterSeconds,omitempty"`
	Rotation            string      `json:"rotation,omitempty"`
	Devices             *UsbDevices `json:"devices,omitempty"`
	UsbConfig           *UsbConfig  `json:"usbConfig,omitempty"`
	FileTransferEnabled *bool       `json:"fileTransferEnabled,omitempty"`
	TimeoutMs           int64       `json:"timeoutMs,omitempty"`
}

var reVIDPID = regexp.MustCompile(`^0x[0-9a-fA-F]{4}$`)

// ValidateUsbConfig enforces the VID/PID shape.
func ValidateUsbConfig(config UsbConfig) error {
	for _, field := range []struct{ name, value string }{
		{"vendor_id", config.VendorID},
		{"product_id", config.ProductID},
	} {
		if !reVIDPID.MatchString(field.value) {
			return fmt.Errorf("Use format 0x0000. (%s)", field.name)
		}
	}
	return nil
}

func record(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func number(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

// ApplyDeviceSettingsAction reads or applies one settings action.
func ApplyDeviceSettingsAction(ctx context.Context, client RPCClient, request Request) (map[string]any, error) {
	switch request.Action {
	case "get":
		type rpcResult struct {
			key string
			val any
			err error
		}
		results := make(chan rpcResult, 4)
		run := func(key, method string) {
			go func() {
				val, err := client.RPC(ctx, method, map[string]any{}, request.TimeoutMs)
				results <- rpcResult{key: key, val: val, err: err}
			}()
		}
		run("guiSettings", "getGuiSettings")
		run("usbDevices", "getUsbDevices")
		run("usbConfig", "getUsbConfig")
		run("fileTransfer", "getFileTransfer")
		out := map[string]any{"action": request.Action}
		for i := 0; i < 4; i++ {
			r := <-results
			if r.err != nil {
				return nil, r.err
			}
			out[r.key] = r.val
		}
		// usbConfig is surfaced under its own "usbConfig" key.
		out["usbConfig"] = out["usbConfig"]
		return out, nil
	case "set_backlight":
		if request.MaxBrightness == nil && request.DimAfterSeconds == nil && request.OffAfterSeconds == nil {
			return nil, fmt.Errorf("set_backlight requires at least one of maxBrightness, dimAfterSeconds, offAfterSeconds")
		}
		current, err := client.RPC(ctx, "getGuiSettings", map[string]any{}, request.TimeoutMs)
		if err != nil {
			return nil, err
		}
		cur := record(current)
		next := map[string]any{
			"max_brightness": numberOrDefault(request.MaxBrightness, cur["max_brightness"]),
			"dim_after":      numberOrDefault(request.DimAfterSeconds, cur["dim_after"]),
			"off_after":      numberOrDefault(request.OffAfterSeconds, cur["off_after"]),
		}
		if _, err := client.RPC(ctx, "setBacklightSettings", next, request.TimeoutMs); err != nil {
			return nil, err
		}
		backlight, err := client.RPC(ctx, "getGuiSettings", map[string]any{}, request.TimeoutMs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"action": request.Action, "applied": true, "backlight": backlight}, nil
	case "set_display_rotation":
		if request.Rotation == "" {
			return nil, fmt.Errorf("set_display_rotation requires rotation")
		}
		if _, err := client.RPC(ctx, "setDisplayRotation", map[string]any{"rotation": request.Rotation}, request.TimeoutMs); err != nil {
			return nil, err
		}
		return map[string]any{"action": request.Action, "applied": true, "rotation": request.Rotation}, nil
	case "set_usb_devices":
		if request.Devices == nil {
			return nil, fmt.Errorf("set_usb_devices requires devices")
		}
		// Device setUsbDevices replaces the whole UsbDevicesState; omitting
		// network_adapter would serde-default to enable=false. Merge like
		// backlight.
		current, err := client.RPC(ctx, "getUsbDevices", map[string]any{}, request.TimeoutMs)
		if err != nil {
			return nil, err
		}
		devicesJSON, err := jsonMarshal(request.Devices)
		if err != nil {
			return nil, err
		}
		devices := record(mustUnmarshal(devicesJSON))
		cur := record(current)
		if _, hasAdapter := devices["network_adapter"]; !hasAdapter {
			if adapter := cur["network_adapter"]; adapter != nil {
				devices["network_adapter"] = adapter
			} else {
				devices["network_adapter"] = map[string]any{
					"enable":       false,
					"adapter_type": "ncm",
					"l3_config": map[string]any{
						"ip_address":  "172.16.0.1",
						"subnet_mask": "255.255.255.0",
					},
				}
			}
		}
		if _, err := client.RPC(ctx, "setUsbDevices", map[string]any{"devices": devices}, request.TimeoutMs); err != nil {
			return nil, err
		}
		usbDevices, err := client.RPC(ctx, "getUsbDevices", map[string]any{}, request.TimeoutMs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"action": request.Action, "applied": true, "usbDevices": usbDevices}, nil
	case "set_usb_config":
		if request.UsbConfig == nil {
			return nil, fmt.Errorf("set_usb_config requires usbConfig")
		}
		if _, err := client.RPC(ctx, "setUsbConfig", request.UsbConfig, request.TimeoutMs); err != nil {
			return nil, err
		}
		usbConfig, err := client.RPC(ctx, "getUsbConfig", map[string]any{}, request.TimeoutMs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"action": request.Action, "applied": true, "usbConfig": usbConfig}, nil
	case "set_file_transfer":
		if request.FileTransferEnabled == nil {
			return nil, fmt.Errorf("set_file_transfer requires fileTransferEnabled")
		}
		if _, err := client.RPC(ctx, "setFileTransfer", map[string]any{"enabled": *request.FileTransferEnabled}, request.TimeoutMs); err != nil {
			return nil, err
		}
		fileTransfer, err := client.RPC(ctx, "getFileTransfer", map[string]any{}, request.TimeoutMs)
		if err != nil {
			return nil, err
		}
		return map[string]any{"action": request.Action, "applied": true, "fileTransfer": fileTransfer}, nil
	}
	return nil, fmt.Errorf("Invalid device settings action: %s", request.Action)
}

func numberOrDefault(override *float64, current any) float64 {
	if override != nil {
		return *override
	}
	return number(current)
}
