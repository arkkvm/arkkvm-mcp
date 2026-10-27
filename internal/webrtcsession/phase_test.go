package webrtcsession

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestDecodeAnswerPayloadBase64String(t *testing.T) {
	inner, _ := json.Marshal(sdpPayload{Type: "answer", SDP: "v=0\r\no=- 1 2 IN IP4 127.0.0.1"})
	encoded := base64.StdEncoding.EncodeToString(inner)
	raw, _ := json.Marshal(encoded)
	sd, err := decodeAnswerPayload(raw)
	if err != nil {
		t.Fatalf("decode base64 answer: %v", err)
	}
	if sd.Type != "answer" || sd.SDP != "v=0\r\no=- 1 2 IN IP4 127.0.0.1" {
		t.Fatalf("unexpected payload: %+v", sd)
	}
}

func TestDecodeAnswerPayloadPlainObject(t *testing.T) {
	raw, _ := json.Marshal(sdpPayload{Type: "answer", SDP: "v=0"})
	sd, err := decodeAnswerPayload(raw)
	if err != nil {
		t.Fatalf("decode plain object answer: %v", err)
	}
	if sd.Type != "answer" || sd.SDP != "v=0" {
		t.Fatalf("unexpected payload: %+v", sd)
	}
}

func TestDecodeAnswerPayloadInvalidBase64(t *testing.T) {
	raw, _ := json.Marshal("!!!not-base64!!!")
	if _, err := decodeAnswerPayload(raw); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDecodeAnswerPayloadWrongShape(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"type": 123})
	if _, err := decodeAnswerPayload(raw); err == nil {
		t.Fatal("expected error for wrong payload shape")
	}
}

func TestDispatchRPCMessageNotification(t *testing.T) {
	var gotMethod string
	var gotParams any
	s := New(Deps{
		BaseURL: "http://127.0.0.1/",
		OnRPCNotification: func(method string, params any) {
			gotMethod = method
			gotParams = params
		},
	})
	s.generation = 1
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "controlConflict",
		"params": map[string]any{
			"owner":  "user",
			"reason": "input",
		},
	})
	s.dispatchRPCMessage(1, payload)
	if gotMethod != "controlConflict" {
		t.Fatalf("method = %q", gotMethod)
	}
	params, ok := gotParams.(map[string]any)
	if !ok || params["owner"] != "user" || params["reason"] != "input" {
		t.Fatalf("params = %#v", gotParams)
	}
}

func TestDispatchRPCMessageIgnoresStaleGeneration(t *testing.T) {
	called := false
	s := New(Deps{
		BaseURL: "http://127.0.0.1/",
		OnRPCNotification: func(method string, params any) {
			called = true
		},
	})
	s.generation = 2
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "controlChanged", "params": map[string]any{}})
	s.dispatchRPCMessage(1, payload)
	if called {
		t.Fatal("stale generation must not dispatch")
	}
}

func TestDispatchRPCMessageErrorDataCode(t *testing.T) {
	s := New(Deps{BaseURL: "http://127.0.0.1/"})
	s.generation = 1
	ch := make(chan rpcOutcome, 1)
	s.pending[7] = ch
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"error": map[string]any{
			"code":    -32000,
			"message": "denied",
			"data":    map[string]any{"code": "controlDenied"},
		},
	})
	s.dispatchRPCMessage(1, payload)
	out := <-ch
	if out.err == nil || out.err.Error() != "-32000: denied (controlDenied)" {
		t.Fatalf("err = %v", out.err)
	}
}
