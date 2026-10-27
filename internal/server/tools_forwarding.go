package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"arkkvm-mcp/internal/forwarding"
)

const ipForwardSchemaTail = `` +
	`"protocol":{"type":"string","enum":["tcp","udp"],"description":"Transport. Default tcp."},` +
	`"localPort":{"type":"integer","minimum":0,"maximum":65535,"description":"Local listen port; 0 or omit for ephemeral."}`

func (s *Server) registerForwardingTools() {
	s.registerIpForwardTool("kvm_ip_forward_usb",
		"Forward a local TCP/UDP port on the controller to the HDMI-controlled machine through ArkKVM USB networking. Preferred path for the controlled host: do NOT pass a host IP — the device resolves the peer from its USB DHCP lease. Prerequisites: enable the USB network adapter on the device. On failure, tell the user they can enable USB network; using kvm_ip_forward_host as fallback requires explicit user confirmation that the host is the controlled machine. Actions: open (returns localHost/localPort), status, close. After open, use standard clients against 127.0.0.1:localPort (ssh, curl, etc.).",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["open","status","close"],"description":"open starts a local listener; status/close need sessionId"},`+
			`"sessionId":{"type":"string","format":"uuid","description":"Session ID from open; required for status/close"},`+
			`"port":{"type":"integer","minimum":1,"maximum":65535,"description":"Target port on the controlled machine. Required for open."},`+
			ipForwardSchemaTail+`},"required":["action"],"additionalProperties":false}`,
		forwarding.ModeUSB)

	s.registerIpForwardTool("kvm_ip_forward_host",
		"Forward a local TCP/UDP port on the controller to an explicit host reachable from the ArkKVM device (board LAN/VLAN/Tailscale, etc.). Use directly ONLY when the user explicitly wants a target that is NOT the HDMI-controlled machine. For the controlled machine, prefer kvm_ip_forward_usb first; use this tool as fallback only after user confirmation that host is the controlled-machine address. Actions: open, status, close.",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["open","status","close"],"description":"open starts a local listener; status/close need sessionId"},`+
			`"sessionId":{"type":"string","format":"uuid","description":"Session ID from open; required for status/close"},`+
			`"host":{"type":"string","minLength":1,"description":"Host reachable from the KVM device. Required for open."},`+
			`"port":{"type":"integer","minimum":1,"maximum":65535,"description":"Target port. Required for open."},`+
			ipForwardSchemaTail+`},"required":["action"],"additionalProperties":false}`,
		forwarding.ModeHost)
}

// classifiedForwardingResult returns the classified failure as pretty JSON
// in both content and structuredContent with isError.
func classifiedForwardingResult(classified forwarding.ClassifiedError) (*mcp.CallToolResult, error) {
	payload := map[string]any{"success": false}
	if classified.Message != "" {
		payload["message"] = classified.Message
	}
	if classified.Code != "" {
		payload["code"] = classified.Code
	}
	if classified.HintEnableUsbNetwork {
		payload["hintEnableUsbNetwork"] = true
	}
	if classified.HintHostFallbackNeedsUserConfirm {
		payload["hintHostFallbackNeedsUserConfirm"] = true
	}
	pretty, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(pretty)}},
		StructuredContent: payload,
		IsError:           true,
	}, nil
}

func (s *Server) registerIpForwardTool(name, description, schema string, mode forwarding.Mode) {
	register(s.mcp, name, description, schema,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			// Client construction failures propagate as plain
			// errors, not classified forwarding failures.
			client, err := s.getClient(name)
			if err != nil {
				return nil, err
			}
			action, _ := argString(args, "action")
			sessionID, hasSessionID := argString(args, "sessionId")
			port, hasPort := argInt(args, "port")
			protocol := ""
			if value, ok := argString(args, "protocol"); ok {
				protocol = value
			}
			host, _ := argString(args, "host")
			var localPort *int
			if value, ok := argInt(args, "localPort"); ok {
				v := int(value)
				localPort = &v
			}

			switch action {
			case "open":
				if mode == forwarding.ModeUSB {
					if !hasPort {
						return nil, fmt.Errorf("kvm_ip_forward_usb open requires port")
					}
				} else if host == "" || !hasPort {
					return nil, fmt.Errorf("kvm_ip_forward_host open requires host and port")
				}
				result, openErr := client.OpenIPForward(ctx, forwarding.OpenParams{
					Mode:      mode,
					Host:      host,
					Port:      int(port),
					Protocol:  forwarding.Protocol(protocol),
					LocalPort: localPort,
				}, 30_000)
				if openErr != nil {
					return classifiedForwardingResult(forwarding.ClassifyError(openErr))
				}
				return resultCallResult(result)
			case "status", "close":
				if !hasSessionID {
					return nil, fmt.Errorf("%s %s requires sessionId", name, action)
				}
			default:
				return nil, fmt.Errorf("%s requires action (open/status/close)", name)
			}
			if action == "status" {
				status, statusErr := client.IPForwardStatus(sessionID)
				if statusErr != nil {
					return classifiedForwardingResult(forwarding.ClassifyError(statusErr))
				}
				return resultCallResult(status)
			}
			if err := client.CloseIPForward(sessionID); err != nil {
				return classifiedForwardingResult(forwarding.ClassifyError(err))
			}
			return resultCallResult(map[string]any{"sessionId": sessionID, "closed": true})
		})
}
