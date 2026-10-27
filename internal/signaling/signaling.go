// Package signaling holds the WebSocket signaling frame shapes for both the
// device LAN path and the cloud relay. Cloud stamps mirror
// arkkvm-cloud/docs/ws/MULTI_CLIENT_SIGNALING.md.
package signaling

import "sync"

// ParseCloudClientId extracts the cloud client id from a device-metadata or
// answer frame, accepting both snake_case and camelCase.
func ParseCloudClientId(message map[string]any) (string, bool) {
	return parseOptionalStringKey(message, "client_id", "clientId")
}

// ParseCloudSessionId extracts the relay session id from an answer frame.
func ParseCloudSessionId(message map[string]any) (string, bool) {
	return parseOptionalStringKey(message, "session_id", "sessionId")
}

func parseOptionalStringKey(message map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := message[k]; ok {
			if s, ok := v.(string); ok && len(s) > 0 {
				return s, true
			}
		}
	}
	return "", false
}

// BuildCloudOfferFrame builds the cloud offer frame with the caller's
// clientType and, once known, the client_id stamp from device-metadata.
func BuildCloudOfferFrame(sd, clientType string, clientId string) map[string]any {
	data := map[string]any{"sd": sd, "clientType": clientType}
	frame := map[string]any{"type": "offer", "data": data}
	if clientId != "" {
		frame["client_id"] = clientId
	}
	return frame
}

// BuildCloudIceFrame builds a new-ice-candidate frame with the cloud relay
// stamps so the server can route the candidate to the right session.
func BuildCloudIceFrame(candidate any, clientId, sessionId string) map[string]any {
	frame := map[string]any{"type": "new-ice-candidate", "data": candidate}
	if clientId != "" {
		frame["client_id"] = clientId
	}
	if sessionId != "" {
		frame["session_id"] = sessionId
	}
	return frame
}

// OutboundIceGate holds local ICE candidates until a cloud session_id is known.
// When requireSessionID is false (LAN), Push always returns the candidate
// immediately — device LAN WS routes by socket id, not top-level session_id.
type OutboundIceGate struct {
	mu               sync.Mutex
	requireSessionID bool
	pending          []any
}

// NewOutboundIceGate builds a gate. Pass requireSessionID=true for cloud relay.
func NewOutboundIceGate(requireSessionID bool) *OutboundIceGate {
	return &OutboundIceGate{requireSessionID: requireSessionID}
}

// Push accepts a local candidate. Ready-to-send candidates are returned;
// otherwise they are queued until Flush after session_id arrives.
func (g *OutboundIceGate) Push(candidate any, sessionID string) []any {
	if g == nil {
		return []any{candidate}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.requireSessionID || sessionID != "" {
		return []any{candidate}
	}
	g.pending = append(g.pending, candidate)
	return nil
}

// Flush drains held candidates once session_id is available (or when the gate
// does not require one). Returns nil when still waiting or the queue is empty.
func (g *OutboundIceGate) Flush(sessionID string) []any {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.requireSessionID && sessionID == "" {
		return nil
	}
	if len(g.pending) == 0 {
		return nil
	}
	out := g.pending
	g.pending = nil
	return out
}

// Clear drops any held candidates (reconnect / teardown).
func (g *OutboundIceGate) Clear() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.pending = nil
	g.mu.Unlock()
}
