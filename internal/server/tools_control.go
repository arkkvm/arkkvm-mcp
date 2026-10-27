package server

import (
	"context"

	"arkkvm-mcp/internal/protocol"
)

func (s *Server) registerControlTools() {
	register(s.mcp, "kvm_get_control",
		"Read who currently holds ArkKVM HID control (user vs agent) and whether a controlConflict arbitration is pending. Use this before keyboard/mouse when a human may be sharing the device, and after controlConflict/controlChanged events.",
		schemaless,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			client, err := s.getClient("kvm_get_control")
			if err != nil {
				return nil, err
			}
			state, err := client.GetControlState(ctx)
			if err != nil {
				return nil, err
			}
			return resultCallResult(controlStatePayload(state))
		})

	register(s.mcp, "kvm_request_control",
		"Ask the ArkKVM device for HID control for this agent session. If a human already holds control, the device starts arbitration: the human sees a WebUI popup and may hand over control, otherwise control transfers automatically after 120s of silence. The requester can never grant its own request (the device refuses with controlDenied). If this agent already owns control, returns the current state. On conflict, pause the task and poll kvm_get_control until owner=agent.",
		schemaless,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			client, err := s.getClient("kvm_request_control")
			if err != nil {
				return nil, err
			}
			state, err := client.RequestControl(ctx)
			if err != nil {
				return nil, err
			}
			return resultCallResult(controlStatePayload(state))
		})

	register(s.mcp, "kvm_deny_control",
		"Withdraw this agent's own pending control request (pauses the task and keeps the current owner). Optional requestId must match the pending arbitration id; omit it to deny the current pending request.",
		`{"type":"object","properties":{`+
			`"requestId":{"type":"string","description":"Pending arbitration requestId. Omit to deny the current pending request."}`+
			`},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			requestID, _ := argString(args, "requestId")
			client, err := s.getClient("kvm_deny_control")
			if err != nil {
				return nil, err
			}
			state, err := client.DenyControl(ctx, requestID)
			if err != nil {
				return nil, err
			}
			return resultCallResult(controlStatePayload(state))
		})

	register(s.mcp, "kvm_release_control",
		"Release HID control held by this agent so a connected human (or vacant lease) can take it. Prefer this when the agent finishes a task and wants to return control.",
		schemaless,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			client, err := s.getClient("kvm_release_control")
			if err != nil {
				return nil, err
			}
			state, err := client.ReleaseControl(ctx)
			if err != nil {
				return nil, err
			}
			return resultCallResult(controlStatePayload(state))
		})
}

func controlStatePayload(state protocol.ControlState) map[string]any {
	payload := map[string]any{
		"owner":   nil,
		"pending": nil,
	}
	if state.Owner != nil {
		payload["owner"] = *state.Owner
	}
	if state.Pending != nil {
		payload["pending"] = state.Pending
	}
	if state.Reason != "" {
		payload["reason"] = state.Reason
	}
	return payload
}
