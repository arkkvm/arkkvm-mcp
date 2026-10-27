package device

import (
	"testing"
)

func TestApplyControlStateCachesOwnerAndPending(t *testing.T) {
	c := &Client{}
	state := c.applyControlState(map[string]any{
		"owner": "user",
		"pending": map[string]any{
			"requestId":     "req-1",
			"requester":     "agent",
			"reason":        "input",
			"deadlineMs":    float64(123),
			"timeoutPolicy": "grant_agent",
		},
		"reason": "conflict",
	})
	if state.Owner == nil || *state.Owner != "user" {
		t.Fatalf("owner = %v, want user", state.Owner)
	}
	if state.Pending == nil || state.Pending.RequestID != "req-1" || state.Pending.Requester != "agent" {
		t.Fatalf("pending = %+v", state.Pending)
	}
	if state.Reason != "conflict" {
		t.Fatalf("reason = %q", state.Reason)
	}

	c.handleRPCNotification("controlChanged", map[string]any{"owner": "agent", "pending": nil})
	cached := c.GetCachedControlState()
	if cached.Owner == nil || *cached.Owner != "agent" {
		t.Fatalf("cached owner after controlChanged = %v", cached.Owner)
	}
	if cached.Pending != nil {
		t.Fatalf("pending should clear, got %+v", cached.Pending)
	}
}

func TestHandleRPCNotificationIgnoresUnknownMethods(t *testing.T) {
	c := &Client{}
	c.applyControlState(map[string]any{"owner": "agent"})
	c.handleRPCNotification("somethingElse", map[string]any{"owner": "user"})
	cached := c.GetCachedControlState()
	if cached.Owner == nil || *cached.Owner != "agent" {
		t.Fatalf("owner must stay agent, got %v", cached.Owner)
	}
}
