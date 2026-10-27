package signaling

import "testing"

func TestParseCloudIds(t *testing.T) {
	if id, ok := ParseCloudClientId(map[string]any{"client_id": "c-1"}); !ok || id != "c-1" {
		t.Errorf("snake_case client_id not parsed: %q %v", id, ok)
	}
	if id, ok := ParseCloudClientId(map[string]any{"clientId": "c-2"}); !ok || id != "c-2" {
		t.Errorf("camelCase clientId not parsed: %q %v", id, ok)
	}
	if _, ok := ParseCloudClientId(map[string]any{"client_id": ""}); ok {
		t.Error("empty id must not be reported as present")
	}
	if _, ok := ParseCloudSessionId(map[string]any{"session_id": 42}); ok {
		t.Error("non-string id must not be reported as present")
	}
}

func TestBuildCloudOfferFrame(t *testing.T) {
	frame := BuildCloudOfferFrame("sdp-body", "agent", "client-7")
	if frame["type"] != "offer" || frame["client_id"] != "client-7" {
		t.Errorf("offer frame = %v", frame)
	}
	data := frame["data"].(map[string]any)
	if data["sd"] != "sdp-body" || data["clientType"] != "agent" {
		t.Errorf("offer data = %v", data)
	}
	frame = BuildCloudOfferFrame("sdp", "agent", "")
	if _, has := frame["client_id"]; has {
		t.Error("client_id must be omitted when unknown")
	}
}

func TestBuildCloudIceFrame(t *testing.T) {
	frame := BuildCloudIceFrame("candidate", "c", "s")
	if frame["type"] != "new-ice-candidate" || frame["data"] != "candidate" ||
		frame["client_id"] != "c" || frame["session_id"] != "s" {
		t.Errorf("ice frame = %v", frame)
	}
}

func TestOutboundIceGateCloudHoldFlush(t *testing.T) {
	gate := NewOutboundIceGate(true)
	if ready := gate.Push("a", ""); len(ready) != 0 {
		t.Fatalf("expected hold without session_id, got %v", ready)
	}
	if ready := gate.Push("b", ""); len(ready) != 0 {
		t.Fatalf("expected hold without session_id, got %v", ready)
	}
	held := gate.Flush("sid-1")
	if len(held) != 2 || held[0] != "a" || held[1] != "b" {
		t.Fatalf("flush = %v", held)
	}
	if again := gate.Flush("sid-1"); len(again) != 0 {
		t.Fatalf("second flush should be empty: %v", again)
	}
}

func TestOutboundIceGateCloudImmediateAfterSession(t *testing.T) {
	gate := NewOutboundIceGate(true)
	ready := gate.Push("c", "sid-1")
	if len(ready) != 1 || ready[0] != "c" {
		t.Fatalf("ready = %v", ready)
	}
}

func TestOutboundIceGateLANPassthrough(t *testing.T) {
	gate := NewOutboundIceGate(false)
	ready := gate.Push("d", "")
	if len(ready) != 1 || ready[0] != "d" {
		t.Fatalf("LAN must not buffer: %v", ready)
	}
	if held := gate.Flush(""); len(held) != 0 {
		t.Fatalf("flush should be empty: %v", held)
	}
}

func TestOutboundIceGateClear(t *testing.T) {
	gate := NewOutboundIceGate(true)
	_ = gate.Push("e", "")
	gate.Clear()
	if held := gate.Flush("sid"); len(held) != 0 {
		t.Fatalf("clear must drop pending: %v", held)
	}
}
