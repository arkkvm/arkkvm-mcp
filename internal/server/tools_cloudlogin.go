package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"arkkvm-mcp/internal/cloudlogin"
	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/logger"
)

func (s *Server) registerCloudLoginTool() {
	register(s.mcp, "kvm_cloud_login",
		"Obtain or clear the cloud web session used by cloud relay mode, without the MCP ever seeing your Google/GitHub credentials. The login UI is the cloud's own page (app origin); this tool runs a one-shot loopback listener on 127.0.0.1 that catches the OIDC redirect and stores the session token.\n\n"+
			"ACTIONS\n"+
			"- login — requires cloudUrl only (kvm_configure action=set); NO deviceId is needed to log in. Opens the cloud login page in the system browser: it first shows a consent card (\"Allow local ArkKVM MCP to access your account\") and, after you allow it, the provider buttons. The cloud callback returns a one-time grant code which this tool exchanges (POST /mcp/token) for the dedicated long-lived MCP token — independent of your browser session (no single-session eviction), revocable via the cloud POST /mcp/revoke. Clouds predating the grant flow still redirect with a web-session token; those are captured as kind:'session' with an upgrade hint. Waits (default 300s) for the /ad-login callback, then applies the token and re-enters cloud relay mode. pairing=true is DEPRECATED: it adds cloudDeviceId for a pairing-shaped flow — WARNING: for an already-adopted device this makes the cloud mint a pairing token, which can unbind the device from your account until re-paired. persist=true additionally writes the token (with cloudUrl/cloudDeviceId/cloudDeviceName/kind) to ~/.config/arkkvm-mcp/cloud-token.json (0600) so later MCP processes re-seed from it; env ARKKVM_CLOUD_* always wins over the file. The login-page origin defaults to https://app.arkkvm.com for the global cloud; api.<domain> clouds derive app.<domain>, so api.arkkvm.cn derives https://app.arkkvm.cn for China, while unrelated hosts fall back to the canonical global app origin. If the user specifies an origin — appUrl here, or ARKKVM_CLOUD_APP_URL — it always wins, so pass it whenever the login page lives anywhere else. openBrowser=false returns the login URL instead of opening a browser (the call still waits for the callback). wait=false returns the login URL immediately and applies the token in the background — poll action=status (it then reports the account device list: a single device is auto-picked, with several devices the result carries a nextStep telling you to pick one and set cloudDeviceId); the browser must run on this machine for the 127.0.0.1 callback to arrive. On success without a configured cloudDeviceId the account's device list is fetched: a single device is auto-selected (the result reports it), otherwise the list is returned to pick from.\n"+
			"- devices — requires cloudUrl and a token (from a previous login or kvm_configure). Lists the account's devices (id/name/online) — use this when you don't know your deviceId. When no cloudDeviceId is configured yet and the account has exactly one device, it is auto-selected and the result reports it; with several devices pick one and run kvm_configure action=set cloudDeviceId=<id>. With a cloudDeviceId already configured the list is informational (no re-pick).\n"+
			"- status — cloud mode, token presence, capture time, the resolved cloud device name (cloudDeviceName), pending/background login progress, and token sources (env / persisted file), without revealing the token value. After a background login it also reports the fetched device list (login.devicePickup) and a top-level nextStep: multiple devices → pick the target and run kvm_configure action=set cloudDeviceId=<deviceId>; unclear which → ask the user.\n"+
			"- logout — clears the runtime token and deletes the persisted token file; cloudUrl/cloudDeviceId stay configured.\n\n"+
			"First-time flow (nothing configured): kvm_configure action=set cloudUrl=<your cloud API> → kvm_cloud_login action=login (persist=true recommended) → kvm_cloud_login action=devices → kvm_configure action=set cloudDeviceId=<picked id> → call any tool.\n\n"+
			"The agent slot is single-occupancy: a cloud agent session and a device-direct agent session are mutually exclusive (agentSlotOccupied). Headless controllers without any browser — or clouds that keep the browser on their own app after login — should use the manual path instead: sign in, then on the signed-in cloud page run localStorage.getItem('token') in DevTools and call kvm_configure action=set cloudUrl=<api> cloudToken=<value>.",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["login","devices","status","logout"],"description":"login starts the loopback OIDC handoff; devices lists the account's devices; status reports cloud session state; logout clears the stored token."},`+
			`"persist":{"type":"boolean","description":"login: also persist the captured token to ~/.config/arkkvm-mcp/cloud-token.json (0600) for later MCP processes. Default false. Env ARKKVM_CLOUD_* takes precedence over the file."},`+
			`"appUrl":{"type":"string","description":"login: cloud app origin hosting the login page. Default https://app.arkkvm.com (global); api.<domain> clouds derive app.<domain> (api.arkkvm.cn → https://app.arkkvm.cn for China), and unrelated hosts fall back to the canonical global app origin; an explicitly specified appUrl — or the ARKKVM_CLOUD_APP_URL env — always wins, so pass it whenever the login page lives anywhere else."},`+
			`"timeoutMs":{"type":"integer","minimum":1000,"maximum":600000,"description":"login: how long to wait for the browser callback. Default 300000 (5 minutes), max 600000."},`+
			`"pairing":{"type":"boolean","description":"login: DEPRECATED — use the default direct login. Adds the pairing-shaped request (deviceId in the URL). WARNING: for an already cloud-adopted device, a pairing-shaped login makes the cloud mint a pairing token, which can put the device back into re-adoption state (it disappears from the account device list until re-paired). Use only when you intend to (re)pair."},`+
			`"openBrowser":{"type":"boolean","description":"login: open the system browser automatically (default true). false just returns loginUrl — the call still waits unless wait=false."},`+
			`"wait":{"type":"boolean","description":"login: wait inside this call for the callback (default true). false returns the login URL immediately and applies the token in the background once the browser callback lands; poll action=status."}`+
			`},"required":["action"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				args, err := decodeArgs(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				action, ok := argString(args, "action")
				if !ok {
					return nil, fmt.Errorf("kvm_cloud_login requires action (login/devices/status/logout)")
				}
				persistFlag, _ := argBool(args, "persist")
				appURL, hasAppURL := argString(args, "appUrl")
				pairing, _ := argBool(args, "pairing")
				openBrowser := true
				if value, ok := argBool(args, "openBrowser"); ok {
					openBrowser = value
				}
				wait := true
				if value, ok := argBool(args, "wait"); ok {
					wait = value
				}
				var timeoutMs int64
				if value, ok := argInt(args, "timeoutMs"); ok {
					timeoutMs = value
				}

				switch action {
				case "devices":
					settings := s.Store.Get()
					if settings.CloudURL == "" {
						return nil, fmt.Errorf("kvm_cloud_login devices needs cloudUrl first: kvm_configure action=set cloudUrl=https://api.arkkvm.com (global) or https://api.arkkvm.cn (China).")
					}
					if settings.CloudToken == "" {
						return nil, fmt.Errorf("No cloud session token yet: run kvm_cloud_login action=login first, or kvm_configure action=set cloudToken=<token>.")
					}
					budget := timeoutMs
					if budget == 0 {
						budget = 15_000
					}
					pickup, err := s.fetchDevicesAndAutoPickSingle(ctx, budget)
					if err != nil {
						return nil, err
					}
					rows := pickup.Rows
					payload := map[string]any{
						"count":   len(rows),
						"devices": rows,
					}
					if pickup.AutoPickedDeviceID != "" {
						payload["autoPickedDeviceId"] = pickup.AutoPickedDeviceID
						payload["note"] = "Only one device on the account — selected automatically; every tool now connects through the cloud relay. Override with kvm_configure action=set cloudDeviceId=<id> if needed."
					}
					// With a deviceId already configured the list is
					// informational: no auto-pick happens and telling the
					// user to pick again would undo it.
					switch {
					case len(rows) == 0:
						payload["nextStep"] = "No devices on this account; pair one from the device web UI first."
					case pickup.AutoPickedDeviceID == "" && s.Store.Get().CloudDeviceID == "":
						payload["nextStep"] = fmt.Sprintf("The account has %d devices: pick the target one and run kvm_configure action=set cloudDeviceId=<deviceId> to finish cloud relay setup; if it is unclear which device to use, ask the user.", len(rows))
					}
					return payload, nil

				case "status":
					cloud := s.Store.PublicConfig().KvmAPI.Cloud
					persisted := config.ReadPersistedCloudToken()
					var tokenAgeMinutes any
					if cloud.TokenCapturedAt != "" {
						if captured, err := time.Parse(time.RFC3339, cloud.TokenCapturedAt); err == nil {
							age := int64(time.Since(captured).Minutes())
							if age < 0 {
								age = 0
							}
							tokenAgeMinutes = age
						}
					}
					login := s.cloudLogin.Status()
					// After a background (wait:false) login the agent polls
					// status: this is where a multi-device account must learn
					// that it has to pick a target — the token itself is
					// already applied by then.
					nextStep := ""
					if login.Applied {
						nextStep = cloudlogin.DevicePickupHint(login.DevicePickup, s.Store.Get().CloudDeviceID != "")
					}
					capturedAt := any(nil)
					if cloud.TokenCapturedAt != "" {
						capturedAt = cloud.TokenCapturedAt
					}
					deviceName := any(nil)
					if cloud.DeviceName != "" {
						deviceName = cloud.DeviceName
					}
					var persistedFile any
					if persisted.Token != "" {
						entry := map[string]any{
							"path": config.CloudTokenFilePath(),
						}
						if persisted.Provider != "" {
							entry["provider"] = persisted.Provider
						} else {
							entry["provider"] = nil
						}
						if persisted.CapturedAt != "" {
							entry["capturedAt"] = persisted.CapturedAt
						} else {
							entry["capturedAt"] = nil
						}
						if persisted.CloudDeviceID != "" {
							entry["cloudDeviceId"] = persisted.CloudDeviceID
						} else {
							entry["cloudDeviceId"] = nil
						}
						if persisted.CloudDeviceName != "" {
							entry["cloudDeviceName"] = persisted.CloudDeviceName
						} else {
							entry["cloudDeviceName"] = nil
						}
						persistedFile = entry
					}
					payload := map[string]any{
						"mode":               cloud.Mode,
						"cloudUrl":           cloud.URL,
						"cloudDeviceId":      cloud.DeviceID,
						"cloudDeviceName":    deviceName,
						"tokenConfigured":    cloud.TokenConfigured,
						"envTokenConfigured": envTokenConfigured(),
						"tokenCapturedAt":    capturedAt,
						"tokenAgeMinutes":    tokenAgeMinutes,
						"persistedFile":      persistedFile,
						"login":              login,
					}
					if nextStep != "" {
						payload["nextStep"] = nextStep
					}
					return payload, nil

				case "logout":
					removedFile := config.ClearPersistedCloudToken()
					_, err := s.Store.Update(config.Update{ClearCloudToken: true})
					if err != nil {
						return nil, err
					}
					s.resetClient()
					return map[string]any{
						"cleared":              true,
						"persistedFileRemoved": removedFile,
						"mode":                 s.Store.Mode(),
					}, nil

				case "login":
					settings := s.Store.Get()
					if settings.CloudURL == "" {
						return nil, fmt.Errorf("kvm_cloud_login needs cloudUrl first: kvm_configure action=set cloudUrl=https://api.arkkvm.com (global) or https://api.arkkvm.cn (China).")
					}
					appURLResolved := ""
					if hasAppURL {
						appURLResolved = appURL
					} else {
						appURLResolved = os.Getenv("ARKKVM_CLOUD_APP_URL")
					}
					flowOptions := cloudlogin.FlowOptions{
						CloudAPIURL: settings.CloudURL,
						AppURL:      appURLResolved,
						// Default is a DIRECT login (no deviceId in the URL):
						// pairing-shaped requests are deprecated.
						DeviceID: func() string {
							if pairing {
								return settings.CloudDeviceID
							}
							return ""
						}(),
						TimeoutMs: timeoutMs,
					}
					if !wait {
						state, err := s.cloudLogin.Start(ctx, flowOptions, persistFlag, openBrowser)
						if err != nil {
							return nil, err
						}
						payload := map[string]any{
							"pending":        state.Pending,
							"loginUrl":       state.LoginURL,
							"startedAt":      state.StartedAt,
							"note":           "Complete the login in a browser on this machine (the callback targets 127.0.0.1). The token is applied automatically when the callback lands; poll action=status — it reports the account device list, and when the account has several devices you must pick one with kvm_configure action=set cloudDeviceId=<deviceId>.",
							"manualFallback": cloudlogin.ManualTokenFallback,
						}
						if state.Error != "" {
							payload["error"] = state.Error
						}
						return payload, nil
					}
					flow, err := cloudlogin.StartFlow(ctx, flowOptions)
					if err != nil {
						return nil, err
					}
					browserOpened := false
					if openBrowser {
						browserOpened = cloudlogin.OpenInBrowser(flow.LoginURL)
					}
					outcome := <-flow.Done
					if !outcome.Success || outcome.Token == "" {
						errText := outcome.Error
						if errText == "" {
							errText = "Cloud login did not complete"
						}
						return map[string]any{
							"success":        false,
							"loginUrl":       flow.LoginURL,
							"browserOpened":  browserOpened,
							"timedOut":       outcome.TimedOut,
							"error":          errText,
							"manualFallback": cloudlogin.ManualTokenFallback,
						}, nil
					}
					capturedAt := isoNow()
					token := outcome.Token
					capturedCopy := capturedAt
					if _, err := s.Store.Update(config.Update{CloudToken: &token, CloudTokenCapturedAt: &capturedCopy}); err != nil {
						return nil, err
					}
					s.resetClient()
					// First-time convenience: when no target device is picked
					// yet, attach the account's device list — and when the
					// account has exactly one device, pick it automatically.
					// With a target already configured, resolve its cloud
					// name instead. Both happen BEFORE persisting so the
					// token file carries cloudDeviceId + cloudDeviceName in
					// one write.
					var rows []cloudlogin.CloudDeviceRow
					haveRows := false
					autoPickedDeviceID := ""
					if settings.CloudDeviceID == "" {
						pickup, err := s.fetchDevicesAndAutoPickSingle(ctx, 15_000)
						if err != nil {
							logger.Warn("Device list after login failed", map[string]any{"error": err.Error()})
						} else {
							rows = pickup.Rows
							haveRows = true
							autoPickedDeviceID = pickup.AutoPickedDeviceID
						}
					} else {
						if _, err := s.refreshCloudDeviceName(ctx, 15_000); err != nil {
							logger.Warn("Device name lookup after login failed", map[string]any{"error": err.Error()})
						}
					}
					var persistedFile any
					if persistFlag {
						current := s.Store.Get()
						path, err := config.WritePersistedCloudToken(config.PersistedCloudToken{
							CloudURL:        settings.CloudURL,
							CloudDeviceID:   current.CloudDeviceID,
							CloudDeviceName: current.CloudDeviceName,
							Token:           outcome.Token,
							Provider:        outcome.Provider,
							CapturedAt:      capturedAt,
							Kind:            outcome.Kind,
						})
						if err != nil {
							logger.Warn("Failed to persist cloud token", map[string]any{"error": err.Error()})
						} else {
							persistedFile = path
						}
					}
					notes := make([]string, 0, 4)
					if outcome.Kind == config.TokenKindSession {
						notes = append(notes, "Captured a legacy web-session token (cloud predates the MCP grant flow). Consider upgrading the cloud to migration/15: the dedicated MCP token no longer evicts your browser session.")
					}
					if outcome.Kind == config.TokenKindMCP {
						notes = append(notes, "Granted a dedicated MCP token via the consent page: long-lived, revocable (POST /mcp/revoke), and independent of your browser session.")
					}
					if autoPickedDeviceID != "" {
						notes = append(notes, fmt.Sprintf("Auto-selected the only device on the account (%s); override with kvm_configure action=set cloudDeviceId=<id> if needed.", autoPickedDeviceID))
					}
					notes = append(notes, "The agent slot is single-occupancy: a cloud agent session and a device-direct agent session are mutually exclusive (agentSlotOccupied).")
					provider := any(nil)
					if outcome.Provider != "" {
						provider = outcome.Provider
					}
					tokenKind := any(nil)
					if outcome.Kind != "" {
						tokenKind = outcome.Kind
					}
					payload := map[string]any{
						"success":         true,
						"provider":        provider,
						"tokenKind":       tokenKind,
						"mode":            s.Store.Mode(),
						"tokenConfigured": true,
						"tokenCapturedAt": capturedAt,
						"persistedFile":   persistedFile,
						"note":            joinNotes(notes),
					}
					if autoPickedDeviceID != "" {
						payload["autoPickedDeviceId"] = autoPickedDeviceID
					} else if haveRows && len(rows) > 0 {
						payload["devices"] = rows
						payload["nextStep"] = fmt.Sprintf("The account has %d devices: pick the target one and run kvm_configure action=set cloudDeviceId=<deviceId> to finish cloud relay setup; if it is unclear which device to use, ask the user.", len(rows))
					}
					switch {
					case haveRows && autoPickedDeviceID == "" && len(rows) == 0:
						payload["nextStep"] = "No devices on this account; pair one from the device web UI first."
					default:
						payload["nextStep"] = "Call any tool (for example kvm_rpc method=ping) to connect through the cloud relay with the new token."
					}
					return payload, nil
				}
				return nil, fmt.Errorf("kvm_cloud_login requires action (login/devices/status/logout)")
			})
		})
}

func joinNotes(notes []string) string {
	out := ""
	for i, note := range notes {
		if i > 0 {
			out += " "
		}
		out += note
	}
	return out
}
