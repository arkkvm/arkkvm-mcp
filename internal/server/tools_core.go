package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"arkkvm-mcp/internal/config"
	"arkkvm-mcp/internal/logger"
)

var readOnlyRPCMethods = map[string]bool{
	"ping": true, "getDeviceID": true, "getUSBState": true, "getVideoState": true,
	"getNetworkState": true, "getNetworkSettings": true, "getUsbNetworkLeases": true,
	"getCloudState": true, "getVirtualMediaState": true, "getVirtualMediaList": true,
	"getFileTransferState": true, "getKeyboardLedState": true, "getCurrentVersion": true,
	"getUpdateStatus": true, "getTLSState": true, "getSerialSettings": true,
	"getKeyboardLayout": true, "getMassStorageMode": true, "getDevModeState": true,
	"getControlState": true,
	"getGuiSettings":  true, "getUsbConfig": true, "getFileTransfer": true,
	"getUsbDevices": true, "getKeyboardMacros": true, "getStorageSpace": true,
	"getStorageDetails": true, "listStorageFiles": true, "getFtStorageDetails": true,
	"ftListFile": true,
}

var healthCheckMethods = []string{
	"getDeviceID", "getVideoState", "getUSBState", "getUsbDevices",
	"getVirtualMediaState", "getVirtualMediaList", "getFileTransferState",
	"getCurrentVersion", "getUpdateStatus", "getNetworkState", "getStorageSpace",
	"getStorageDetails", "listStorageFiles", "getMassStorageMode", "getKeyboardLayout",
}

func (s *Server) registerCoreTools() {
	register(s.mcp, "device_status",
		"Read the ArkKVM device setup status over the configured device URL, plus the local WebRTC session snapshot (state/generation/channel readiness). Use this to verify that ARKKVM_URL points to the KVM device and that the device is initialized. HTTP status reports the KVM device only; connection reflects this MCP process WebRTC control plane (idle until a WebRTC tool has connected). Failures return structured errors: DEVICE_NOT_CONFIGURED, DEVICE_AUTH_REQUIRED, CONNECTION_* when applicable.",
		schemaless,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				client, err := s.getClient("device_status")
				if err != nil {
					return nil, err
				}
				deviceStatus, err := client.Status(ctx, 10_000)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"device":     deviceStatus,
					"connection": client.GetConnectionStatus(),
				}, nil
			})
		})

	register(s.mcp, "device_capabilities",
		"Return the capabilities and control boundary of this local ArkKVM MCP adapter. The adapter runs on the controller machine and reaches the ArkKVM device over one of two transports: direct device connection (LAN/VPN, the default) or cloud relay (cloudUrl + cloudToken + cloudDeviceId configured; for devices already adopted by the ArkKVM cloud, reachable over the public internet). Both use WebRTC media plus DataChannels. It exposes device status, read-only device RPC, a device health snapshot, HID control-lease tools (get/request/deny/release) for human/agent arbitration, HDMI screenshots, local HDMI OCR, model-free frame comparison and text waiting, USB HID keyboard/mouse/wheel input plus named key sequences, USB/host IP port forwarding, and the OS-install flow tools: virtual media mount/unmount, storage image upload/delete, and ATX power actions on the controlled machine. The controlled machine is operated only through ArkKVM: this adapter never opens a direct network connection to it. OCR runs locally on the controller against decoded HDMI frames. WebRTC disconnects are not auto-retried; tools return CONNECTION_LOST/CONNECTION_FAILED and the Agent decides whether to call again.",
		schemaless,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			var connection any
			if s.Store.Configured() {
				if client, err := s.getClient("device_status"); err == nil {
					connection = client.GetConnectionStatus()
				}
			}
			publicConfig := s.Store.PublicConfig()
			hdmiTools := make([]string, 0)
			for _, tool := range config.ToolIDs {
				if len(tool) > 5 && tool[:5] == "hdmi_" {
					hdmiTools = append(hdmiTools, tool)
				}
			}
			tools := append([]string{"device_capabilities", "kvm_configure", "kvm_cloud_login"}, config.ToolIDs...)
			return resultCallResult(map[string]any{
				"deviceUrl":  publicConfig.KvmAPI.URL,
				"cloud":      publicConfig.KvmAPI.Cloud,
				"connection": connection,
				"transports": []string{"webrtc-datachannel", "webrtc-media"},
				"tools":      tools,
				"configuration": map[string]any{
					"configured": s.Store.Configured(),
					"storage":    "in-memory",
					"note":       "Settings live only in this MCP process. Use kvm_configure to read or change them; a restarted server is back to the environment defaults — cloud settings re-seed from the token file written by kvm_cloud_login persist=true (env ARKKVM_CLOUD_* wins over the file). On password-authenticated devices, set the device password with kvm_configure and the MCP logs in as the agent role automatically.",
				},
				"video": map[string]any{
					"available": true,
					"format":    "H264",
					"tools":     hdmiTools,
					"note":      "HDMI OCR runs on the controller using the configured local OCR command; graphical tools return decoded PNG frames or objective pixel-difference metrics.",
				},
				"connectionPolicy": map[string]any{
					"autoRetry": false,
					"note":      "On CONNECTION_LOST or CONNECTION_FAILED, the session is torn down. The Agent may retry the same tool; ensureConnected runs on demand without silent re-send loops.",
				},
			})
		})

	s.registerKvmConfigure()
	s.registerKvmRPC()
	s.registerKvmHealthSnapshot()
}

func (s *Server) registerKvmConfigure() {
	register(s.mcp, "kvm_configure",
		"Read or change the KVM connection settings for this MCP process. Settings are held in memory only and this tool never writes them to disk; a restarted MCP server falls back to its environment defaults, except cloud settings (cloudUrl/cloudToken/cloudDeviceId/cloudDeviceName), which additionally re-seed from ~/.config/arkkvm-mcp/cloud-token.json when kvm_cloud_login persist=true wrote it (env ARKKVM_CLOUD_* wins over the file). Exception to memory-only: once a cloudDeviceId is set (and its cloud name resolved), the picked cloudDeviceId/cloudDeviceName are recorded into that token file when it belongs to the same cloud session (same token), so a restarted server keeps the target. Call action=get first when connectivity fails: it reports the current URLs, the cloud device name when one is known (cloud.deviceName, resolved from the account device list), and whether credentials are set, without revealing their values. Device LAN mode (default): set action=set with the device url before any other tool can reach the device. agentCookie and agentCsrfToken are only needed when the device enforces authentication; supply them as the raw cookie value (for example \"authTokenAgent=...\") and the CSRF token separately. Alternatively, on a password-authenticated device set password once: the MCP then logs in as the agent role automatically and obtains the cookie and CSRF token itself, so agentCookie/agentCsrfToken become unnecessary; an explicitly configured agentCookie still takes precedence. Cloud relay mode: set cloudUrl + cloudDeviceId together, then run kvm_cloud_login action=login (or set cloudToken) to reach an already cloud-adopted device through the ArkKVM cloud without VPN — cloudToken is a cloud access token — normally the dedicated MCP token minted by kvm_cloud_login, or the legacy web-session token (the loginToken from the ad-login redirect) on the manual path — and cloudDeviceId is the device id from the app.arkkvm.com (global) or app.arkkvm.cn (China) device page URL. In cloud mode signaling and ICE/TURN come from the cloud and device-local HTTP status is replaced by cloud-relay info; iceServersJson overrides the cloud-provided TURN servers. Changing any setting closes the current WebRTC session so the next tool call reconnects with the new values. action=reset discards the runtime settings and restores the startup defaults (environment variables, plus the persisted cloud token file when present).",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["get","set","reset"],"description":"get reads the current settings; set applies the supplied fields; reset restores the startup defaults (environment variables, plus the persisted cloud token file when present)."},`+
			`"url":{"type":"string","minLength":1,"description":"ArkKVM device base URL, for example http://192.168.12.180. This is the KVM device, not the HDMI-connected controlled machine. Used in device LAN mode."},`+
			`"agentCookie":{"type":"string","description":"Agent-role session cookie sent to the device signaling endpoint. Use the agent cookie only; do not combine it with the user cookie."},`+
			`"agentCsrfToken":{"type":"string","description":"Agent-role CSRF token paired with agentCookie for authenticated devices."},`+
			`"password":{"type":"string","description":"ArkKVM device password for devices with password authentication enabled. The MCP logs in as the agent role (POST /auth/login-local) and obtains the session cookie and CSRF token automatically, so agentCookie/agentCsrfToken are no longer needed. Write-only: action=get reports passwordConfigured instead of the value."},`+
			`"iceServersJson":{"type":"string","description":"ICE servers as a JSON array, for example [{\"urls\":\"stun:127.0.0.1:3478\"}]. Omit to let the device signaling response (device mode) or the cloud (cloud mode) supply them. When set in cloud mode it overrides the cloud-provided TURN servers."},`+
			`"cloudUrl":{"type":"string","minLength":1,"description":"ArkKVM cloud API base URL, for example https://api.arkkvm.com (global) or https://api.arkkvm.cn (China). Set it first; the token then comes from kvm_cloud_login action=login (or cloudToken) and cloudDeviceId from the login auto-pick or this setter — all three together switch every tool to cloud relay mode."},`+
			`"cloudToken":{"type":"string","description":"Cloud token for the relay: prefer the dedicated MCP token minted by kvm_cloud_login (it is what that tool applies); a manually supplied token is the legacy web-session kind (the loginToken from the /ad-login redirect, or the localStorage token on the signed-in cloud page). Write-only: action=get reports tokenConfigured instead of the value."},`+
			`"cloudDeviceId":{"type":"string","minLength":1,"description":"Target device cloud id in cloud relay mode, for example 0123456789abcdef — the id in the app.arkkvm.com (global) or app.arkkvm.cn (China) device page URL. Do not know it? Log in first (kvm_cloud_login action=login), then run kvm_cloud_login action=devices to list your devices. The picked id (and its resolved cloud name) is recorded into the persisted token file when it belongs to the same cloud session, so restarts keep the target."},`+
			`"clearAgentCookie":{"type":"boolean","description":"Remove the stored agent cookie. Use with action=set."},`+
			`"clearAgentCsrfToken":{"type":"boolean","description":"Remove the stored agent CSRF token. Use with action=set."},`+
			`"clearPassword":{"type":"boolean","description":"Remove the stored device password. Use with action=set."},`+
			`"clearIceServers":{"type":"boolean","description":"Remove the stored ICE servers configuration. Use with action=set."},`+
			`"clearCloudUrl":{"type":"boolean","description":"Remove the stored cloud API URL (returns to device LAN mode once all cloud settings are cleared). Use with action=set."},`+
			`"clearCloudToken":{"type":"boolean","description":"Remove the stored cloud session token. Use with action=set."},`+
			`"clearCloudDeviceId":{"type":"boolean","description":"Remove the stored cloud device id. Use with action=set."}`+
			`},"required":["action"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			action, ok := argString(args, "action")
			if !ok {
				return nil, fmt.Errorf("kvm_configure requires action (get/set/reset)")
			}
			switch action {
			case "get":
				return resultCallResult(s.Store.PublicConfig())
			case "reset":
				config := s.Store.ResetToEnvironment()
				s.resetClient()
				return resultCallResult(config)
			case "set":
				update := config.Update{}
				changed := false
				setStr := func(key string, dst **string) {
					if value, ok := argString(args, key); ok {
						copy := value
						*dst = &copy
						changed = true
					}
				}
				setStr("url", &update.URL)
				setStr("agentCookie", &update.AgentCookie)
				setStr("agentCsrfToken", &update.AgentCsrfToken)
				setStr("password", &update.Password)
				setStr("iceServersJson", &update.IceServersJSON)
				setStr("cloudUrl", &update.CloudURL)
				setStr("cloudToken", &update.CloudToken)
				setStr("cloudDeviceId", &update.CloudDeviceID)
				setStr("cloudDeviceName", &update.CloudDeviceName)
				setFlag := func(key string, dst *bool) {
					if value, ok := argBool(args, key); ok {
						*dst = value
						changed = true
					}
				}
				setFlag("clearAgentCookie", &update.ClearAgentCookie)
				setFlag("clearAgentCsrfToken", &update.ClearAgentCsrfToken)
				setFlag("clearPassword", &update.ClearPassword)
				setFlag("clearIceServers", &update.ClearIceServers)
				setFlag("clearCloudUrl", &update.ClearCloudURL)
				setFlag("clearCloudToken", &update.ClearCloudToken)
				setFlag("clearCloudDeviceId", &update.ClearCloudDeviceID)
				if !changed {
					return nil, fmt.Errorf("kvm_configure action=set requires at least one field to change")
				}
				// Validate before dropping the working session, so a rejected
				// value leaves the previous settings and their live WebRTC
				// session untouched.
				publicConfig, err := s.Store.Update(update)
				if err != nil {
					return nil, err
				}
				s.resetClient()
				// A complete cloud triple with a picked target: record the
				// deviceId in the token file (same cloud session only) so
				// restarts keep it.
				current := s.Store.Get()
				if config.ConnectionModeOf(current) == config.ModeCloud && current.CloudDeviceID != "" {
					s.persistCloudDeviceTarget(current.CloudDeviceID, current.CloudDeviceName)
				}
				// A complete cloud triple without a known device name:
				// resolve it in the background — best effort, never blocks.
				if config.ConnectionModeOf(current) == config.ModeCloud && current.CloudDeviceName == "" {
					go func() {
						if _, err := s.refreshCloudDeviceName(context.Background(), 15_000); err != nil {
							logger.Warn("Cloud device name lookup failed", map[string]any{"error": err.Error()})
						}
					}()
				}
				return resultCallResult(publicConfig)
			}
			return nil, fmt.Errorf("kvm_configure requires action (get/set/reset)")
		})
}

func (s *Server) registerKvmRPC() {
	register(s.mcp, "kvm_rpc",
		"Call one allowlisted, read-only ArkKVM device JSON-RPC method through the WebRTC rpc DataChannel. This queries the KVM device control plane, not the HDMI-connected computer; it is not a shell and cannot run commands such as ls, dir, PowerShell, or cmd on the controlled machine. Write operations live in dedicated tools: kvm_device_settings, kvm_virtual_media, kvm_virtual_usb, kvm_storage_upload, kvm_storage_delete, kvm_atx_power.\n\nALLOWED METHODS (read-only; anything else is rejected as RPC_METHOD_NOT_EXPOSED)\n- identity/version: ping, getDeviceID, getCurrentVersion, getUpdateStatus, getTLSState\n- video/input: getVideoState, getKeyboardLedState, getKeyboardLayout, getKeyboardMacros, getSerialSettings\n- network: getNetworkState, getNetworkSettings, getCloudState\n- storage/media: getStorageSpace, getStorageDetails, listStorageFiles, getFtStorageDetails, ftListFile, getVirtualMediaState, getVirtualMediaList, getMassStorageMode, getFileTransferState\n- usb: getUSBState, getUsbDevices, getUsbConfig\n- settings: getGuiSettings, getFileTransfer, getDevModeState\n\nTYPICAL ORDER\n1. device_status first when connectivity is uncertain.\n2. kvm_rpc with the exact method name; pass params only for methods that take them (most take none).\n\nERRORS (structuredContent carries ok=false with a stable code)\n- RPC_METHOD_NOT_EXPOSED — method missing from the list above → use a dedicated tool or report unsupported.\n- RPC_DEVICE_ERROR — device rejected the call; deviceCode/deviceData hold its error and cause.\n- RPC_TIMEOUT — no response within timeoutMs → retry with a larger timeoutMs.\n- DEVICE_NOT_CONFIGURED — no device URL yet → kvm_configure action=set.\n- DEVICE_AUTH_REQUIRED — device enforces password auth and the credentials are missing, stale, or wrong (HTTP 401/403) → kvm_configure action=set password=..., or agentCookie + agentCsrfToken.\n\nEXAMPLE\nkvm_rpc method=getGuiSettings → {method, result:{max_brightness, dim_after, off_after, rotation}}",
		`{"type":"object","properties":{`+
			`"method":{"type":"string","minLength":1,"description":"Exact allowlisted ArkKVM RPC method name, for example ping or getUSBState. This is not a target-machine shell command."},`+
			`"params":{"type":"object","additionalProperties":{},"description":"JSON object containing parameters for the selected allowlisted RPC method. Omit when the method takes no parameters."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"RPC response timeout in milliseconds. Default is 30000."}`+
			`},"required":["method"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				args, err := decodeArgs(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				method, ok := argString(args, "method")
				if !ok || method == "" {
					return nil, fmt.Errorf("kvm_rpc requires method")
				}
				if !readOnlyRPCMethods[method] {
					return nil, fmt.Errorf("RPC method is not exposed by local MCP: %s", method)
				}
				var timeoutMs int64
				if value, ok := argInt(args, "timeoutMs"); ok {
					timeoutMs = value
				}
				params, hasParams := argMap(args, "params")
				if !hasParams {
					params = map[string]any{}
				}
				client, err := s.getClient("kvm_rpc")
				if err != nil {
					return nil, err
				}
				result, err := client.RPC(ctx, method, params, timeoutMs)
				if err != nil {
					return nil, err
				}
				return map[string]any{"method": method, "result": result}, nil
			})
		})
}

func (s *Server) registerKvmHealthSnapshot() {
	register(s.mcp, "kvm_health_snapshot",
		"Collect a one-shot diagnostic snapshot of the ArkKVM device by running a fixed set of read-only checks in parallel: HTTP device status plus device ID, video state, USB state and device flags, virtual media state and list, file transfer state, current version, update status, network state, storage space/details/files, mass storage mode, and keyboard layout. In cloud relay mode this also includes cloudConfig (relay target, token presence and age) and cloudIceServers (cloud-provided ICE/TURN availability, exercising the cloud API auth path). Individual check failures do not abort the snapshot; each check reports ok/result or ok=false/error. Use this before starting an OS install flow to confirm the device, HDMI video, and virtual media are ready.",
		`{"type":"object","properties":{"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"Total budget for all checks in milliseconds. Default 30000."}},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				args, err := decodeArgs(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				timeoutMs := int64(30_000)
				if value, ok := argInt(args, "timeoutMs"); ok {
					timeoutMs = value
				}
				startedAt := time.Now()
				deadline := startedAt.Add(time.Duration(timeoutMs) * time.Millisecond)
				client, err := s.getClient("kvm_health_snapshot")
				if err != nil {
					return nil, err
				}
				var checksMu sync.Mutex
				checks := map[string]any{}
				runCheck := func(name string, operation func(budgetMs int64) (any, error)) {
					budget := time.Until(deadline).Milliseconds()
					if budget < 1 {
						budget = 1
					}
					result, err := operation(budget)
					checksMu.Lock()
					defer checksMu.Unlock()
					if err != nil {
						checks[name] = map[string]any{"ok": false, "error": err.Error()}
						return
					}
					checks[name] = map[string]any{"ok": true, "result": result}
				}
				var wg sync.WaitGroup
				for _, method := range healthCheckMethods {
					wg.Add(1)
					go func(method string) {
						defer wg.Done()
						runCheck(method, func(budgetMs int64) (any, error) {
							return client.RPC(ctx, method, map[string]any{}, budgetMs)
						})
					}(method)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					runCheck("deviceStatus", func(budgetMs int64) (any, error) {
						return client.Status(ctx, budgetMs)
					})
				}()
				cloudSettings := s.Store.Get()
				if config.ConnectionModeOf(cloudSettings) == config.ModeCloud {
					wg.Add(1)
					go func() {
						defer wg.Done()
						runCheck("cloudConfig", func(int64) (any, error) {
							tokenAgeMinutes := any(nil)
							if cloudSettings.CloudTokenCapturedAt != "" {
								if captured, err := time.Parse(time.RFC3339, cloudSettings.CloudTokenCapturedAt); err == nil {
									age := int64(time.Since(captured).Minutes())
									if age < 0 {
										age = 0
									}
									tokenAgeMinutes = age
								}
							}
							capturedAt := any(nil)
							if cloudSettings.CloudTokenCapturedAt != "" {
								capturedAt = cloudSettings.CloudTokenCapturedAt
							}
							return map[string]any{
								"transport":       "cloud-relay",
								"cloudUrl":        cloudSettings.CloudURL,
								"cloudDeviceId":   cloudSettings.CloudDeviceID,
								"tokenConfigured": cloudSettings.CloudToken != "",
								"tokenCapturedAt": capturedAt,
								"tokenAgeMinutes": tokenAgeMinutes,
							}, nil
						})
					}()
					wg.Add(1)
					go func() {
						defer wg.Done()
						runCheck("cloudIceServers", func(budgetMs int64) (any, error) {
							servers, err := client.CloudIceServers(ctx, budgetMs)
							if err != nil {
								return nil, err
							}
							urls := make([]any, 0, len(servers))
							for _, server := range servers {
								if server.URLs != nil {
									urls = append(urls, server.URLs)
								} else {
									urls = append(urls, nil)
								}
							}
							return map[string]any{"serverCount": len(servers), "urls": urls}, nil
						})
					}()
				}
				wg.Wait()
				okCount := 0
				for _, check := range checks {
					if check.(map[string]any)["ok"] == true {
						okCount++
					}
				}
				return map[string]any{
					"elapsedMs":   time.Since(startedAt).Milliseconds(),
					"okCount":     okCount,
					"failedCount": len(checks) - okCount,
					"checks":      checks,
				}, nil
			})
		})
}
