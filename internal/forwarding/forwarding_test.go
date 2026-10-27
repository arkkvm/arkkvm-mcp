package forwarding

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyError(t *testing.T) {
	result := ClassifyError(errors.New("startIpForward failed: [usb_network_disabled]: enable USB network"))
	if result.Code != "usb_network_disabled" || !result.HintEnableUsbNetwork || !result.HintHostFallbackNeedsUserConfirm {
		t.Errorf("usb_network_disabled classification = %+v", result)
	}
	result = ClassifyError(errors.New("device error: [usb_peer_not_leased]"))
	if result.Code != "usb_peer_not_leased" || !result.HintEnableUsbNetwork {
		t.Errorf("usb_peer_not_leased classification = %+v", result)
	}
	result = ClassifyError(errors.New("USB DHCP lease missing"))
	if !result.HintEnableUsbNetwork {
		t.Errorf("prose USB DHCP failure must set hints: %+v", result)
	}
	result = ClassifyError(errors.New("IP forward DataChannel did not open"))
	if result.Code != "" || result.HintEnableUsbNetwork {
		t.Errorf("non-USB failure must stay unclassified: %+v", result)
	}
}

func TestParseProtocol(t *testing.T) {
	if got, err := parseProtocol(" TCP "); err != nil || got != TCP {
		t.Errorf("parseProtocol(TCP) = %v, %v", got, err)
	}
	if got, err := parseProtocol(""); err != nil || got != TCP {
		t.Errorf("parseProtocol(\"\") defaults to tcp: %v, %v", got, err)
	}
	if _, err := parseProtocol("icmp"); err == nil {
		t.Error("icmp must fail")
	}
}

func TestClassifyErrorMessageField(t *testing.T) {
	message := fmt.Sprintf("boom %d", 42)
	result := ClassifyError(errors.New(message))
	if result.Message != message {
		t.Errorf("message must be preserved: %q", result.Message)
	}
}
