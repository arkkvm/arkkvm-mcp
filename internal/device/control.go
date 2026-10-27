package device

import (
	"context"
	"encoding/json"
	"fmt"

	"arkkvm-mcp/internal/logger"
	"arkkvm-mcp/internal/protocol"
)

// GetCachedControlState returns the last observed control-lease snapshot
// (from RPC replies or controlConflict/controlChanged notifications).
func (c *Client) GetCachedControlState() protocol.ControlState {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	return c.snapshotControlStateLocked()
}

// GetControlState queries the device for the current HID control lease.
func (c *Client) GetControlState(ctx context.Context) (protocol.ControlState, error) {
	value, err := c.session.RPC(ctx, "getControlState", map[string]any{}, defaultTimeoutMs)
	if err != nil {
		return protocol.ControlState{}, err
	}
	return c.applyControlState(value), nil
}

// RequestControl asks the device for HID control for this agent session.
func (c *Client) RequestControl(ctx context.Context) (protocol.ControlState, error) {
	value, err := c.session.RPC(ctx, "requestControl", map[string]any{}, defaultTimeoutMs)
	if err != nil {
		return protocol.ControlState{}, err
	}
	return c.applyControlState(value), nil
}

// DenyControl rejects the pending control request (optional requestId).
func (c *Client) DenyControl(ctx context.Context, requestID string) (protocol.ControlState, error) {
	params := map[string]any{}
	if requestID != "" {
		params["requestId"] = requestID
	}
	value, err := c.session.RPC(ctx, "denyControl", params, defaultTimeoutMs)
	if err != nil {
		return protocol.ControlState{}, err
	}
	return c.applyControlState(value), nil
}

// ReleaseControl drops HID control held by this agent.
func (c *Client) ReleaseControl(ctx context.Context) (protocol.ControlState, error) {
	value, err := c.session.RPC(ctx, "releaseControl", map[string]any{}, defaultTimeoutMs)
	if err != nil {
		return protocol.ControlState{}, err
	}
	return c.applyControlState(value), nil
}

// HasControl reports whether this agent role currently holds HID control.
func (c *Client) HasControl(ctx context.Context) (bool, error) {
	state, err := c.GetControlState(ctx)
	if err != nil {
		return false, err
	}
	return state.Owner != nil && *state.Owner == mcpClientType, nil
}

// ensureAgentControl requests the HID lease when this agent does not already
// hold it. This runs before every input report.
func (c *Client) ensureAgentControl(ctx context.Context) error {
	c.controlMu.Lock()
	owns := c.controlOwner != nil && *c.controlOwner == mcpClientType
	c.controlMu.Unlock()
	if owns {
		return nil
	}
	state, err := c.GetControlState(ctx)
	if err != nil {
		return err
	}
	if state.Owner != nil && *state.Owner == mcpClientType {
		return nil
	}
	if state.Pending != nil && state.Pending.Requester == mcpClientType {
		return fmt.Errorf(
			"controlDenied: agent control request pending (%s); wait for grant/deny or timeout",
			state.Pending.RequestID,
		)
	}
	requested, err := c.RequestControl(ctx)
	if err != nil {
		return err
	}
	if requested.Owner != nil && *requested.Owner == mcpClientType {
		return nil
	}
	owner := "none"
	if requested.Owner != nil {
		owner = *requested.Owner
	}
	return fmt.Errorf("controlDenied: agent does not hold control (owner=%s); arbitration started", owner)
}

func (c *Client) handleRPCNotification(method string, params any) {
	if method != "controlChanged" && method != "controlConflict" {
		return
	}
	state := c.applyControlState(params)
	if method == "controlConflict" {
		c.controlMu.Lock()
		c.lastControlConflict = state
		c.controlMu.Unlock()
	}
	logger.Info("Control lease event", map[string]any{
		"method":  method,
		"owner":   ownerOrNil(state.Owner),
		"reason":  state.Reason,
		"pending": state.Pending,
	})
}

func (c *Client) applyControlState(value any) protocol.ControlState {
	parsed := parseControlState(value)
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if value != nil {
		if m, ok := value.(map[string]any); ok {
			if _, has := m["owner"]; has {
				c.controlOwner = parsed.Owner
			}
			if _, has := m["pending"]; has {
				c.controlPending = parsed.Pending
			}
			if reason, ok := m["reason"].(string); ok && reason != "" {
				c.lastControlConflict.Reason = reason
			}
		} else {
			// Typed / round-tripped JSON: always take both fields.
			c.controlOwner = parsed.Owner
			c.controlPending = parsed.Pending
			if parsed.Reason != "" {
				c.lastControlConflict.Reason = parsed.Reason
			}
		}
	}
	return c.snapshotControlStateLocked()
}

func (c *Client) snapshotControlStateLocked() protocol.ControlState {
	var owner *string
	if c.controlOwner != nil {
		copy := *c.controlOwner
		owner = &copy
	}
	var pending *protocol.ControlPending
	if c.controlPending != nil {
		copy := *c.controlPending
		pending = &copy
	}
	return protocol.ControlState{
		Owner:   owner,
		Pending: pending,
		Reason:  c.lastControlConflict.Reason,
	}
}

func parseControlState(value any) protocol.ControlState {
	if value == nil {
		return protocol.ControlState{}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return protocol.ControlState{}
	}
	var state protocol.ControlState
	if err := json.Unmarshal(data, &state); err != nil {
		return protocol.ControlState{}
	}
	return state
}

func ownerOrNil(owner *string) any {
	if owner == nil {
		return nil
	}
	return *owner
}
