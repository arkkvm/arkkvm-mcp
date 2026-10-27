package server

import (
	"context"
	"fmt"

	"arkkvm-mcp/internal/device"
	"arkkvm-mcp/internal/protocol"
	"arkkvm-mcp/internal/settings"
)

func (s *Server) registerMediaTools() {
	register(s.mcp, "kvm_virtual_media",
		"Inspect and control the ArkKVM virtual media (virtual CD-ROM / disk) that is exposed to the HDMI-connected controlled machine over USB. Actions: state reads the current mount (null when nothing is mounted); list reads the saved URL presets; mount_url streams an image over HTTP(S) from a Range-request-capable server (the device mounts it without storing it); mount_storage mounts a file previously uploaded into device storage with kvm_storage_upload; unmount detaches the current media. Mount mode CDROM suits OS installer ISOs. Only one virtual media can be mounted at a time; pass forceUnmount=true to replace an existing mount. After mounting for an install, boot or reboot the controlled machine with kvm_atx_power.",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["state","list","mount_url","mount_storage","unmount"],"description":"Virtual media operation."},`+
			`"url":{"type":"string","minLength":1,"description":"http(s) image URL for mount_url. The remote server must support Range requests (206 responses)."},`+
			`"filename":{"type":"string","minLength":1,"description":"Storage filename for mount_storage, as listed by kvm_health_snapshot listStorageFiles. No path separators."},`+
			`"mode":{"type":"string","enum":["CDROM","DISK"],"description":"Mount mode. CDROM for installer ISOs, DISK for raw disk images. Default CDROM."},`+
			`"forceUnmount":{"type":"boolean","description":"Unmount an already mounted media before mounting. Default false; mounting over an existing media then fails."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":300000,"description":"RPC timeout in milliseconds. Default 30000."}`+
			`},"required":["action"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			action, ok := argString(args, "action")
			if !ok {
				return nil, fmt.Errorf("kvm_virtual_media requires action")
			}
			rawURL, _ := argString(args, "url")
			filename, _ := argString(args, "filename")
			mode := protocol.MediaModeCDROM
			if value, has := argString(args, "mode"); has {
				mode = protocol.VirtualMediaMode(value)
			}
			forceUnmount, _ := argBool(args, "forceUnmount")
			var timeoutMs int64
			if value, has := argInt(args, "timeoutMs"); has {
				timeoutMs = value
			}
			client, err := s.getClient("kvm_virtual_media")
			if err != nil {
				return nil, err
			}
			switch action {
			case "state":
				state, err := client.GetVirtualMediaState(ctx)
				if err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "state": state})
			case "list":
				list, err := client.GetVirtualMediaList(ctx)
				if err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "list": list})
			case "unmount":
				previous, err := client.GetVirtualMediaState(ctx)
				if err != nil {
					return nil, err
				}
				if err := client.UnmountVirtualMedia(ctx, timeoutMs); err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "unmounted": true, "previous": previous})
			}
			if action == "mount_url" && rawURL == "" {
				return nil, fmt.Errorf("mount_url requires url")
			}
			if action == "mount_storage" && filename == "" {
				return nil, fmt.Errorf("mount_storage requires filename")
			}
			current, err := client.GetVirtualMediaState(ctx)
			if err != nil {
				return nil, err
			}
			if currentRecord, has := current.(map[string]any); has && currentRecord != nil {
				description := ""
				if v, ok := currentRecord["filename"].(string); ok && v != "" {
					description = v
				} else if v, ok := currentRecord["url"].(string); ok && v != "" {
					description = v
				} else if v, ok := currentRecord["source"].(string); ok {
					description = v
				}
				currentMode, _ := currentRecord["mode"].(string)
				if !forceUnmount {
					return nil, fmt.Errorf("Another virtual media is already mounted (%s, mode %s). Pass forceUnmount=true to replace it.", description, currentMode)
				}
				if err := client.UnmountVirtualMedia(ctx, timeoutMs); err != nil {
					return nil, err
				}
			}
			if action == "mount_url" {
				if err := client.MountVirtualMediaFromURL(ctx, rawURL, mode, timeoutMs); err != nil {
					return nil, err
				}
			} else {
				if err := client.MountVirtualMediaFromStorage(ctx, filename, mode, timeoutMs); err != nil {
					return nil, err
				}
			}
			state, err := client.GetVirtualMediaState(ctx)
			if err != nil {
				return nil, err
			}
			return resultCallResult(map[string]any{"action": action, "mounted": true, "mode": string(mode), "state": state})
		})

	s.registerStorageUploadTool()
	s.registerStorageDeleteTool()
	s.registerAtxPowerTool()

	register(s.mcp, "kvm_virtual_usb",
		"Inspect and expose the ArkKVM file-transfer (virtual USB drive) filesystem to the HDMI-connected controlled machine. Actions: state reads the current file-transfer mount target (null when the feature is disabled); list lists files in one directory of the device file-transfer volume (the volume mounted at /mnt/ft_disk, including any .iso or disk images placed there); mount exposes the whole file-transfer volume to the controlled machine (target Kvm) or to the remote USB port (target RemoteUsb) as a USB mass-storage disk; unmount detaches it. Files on this volume are exposed as files on a USB disk, which the controlled machine can read after boot; booting an installer requires the firmware to support USB boot, unlike the virtual CD-ROM provided by kvm_virtual_media.",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["state","list","mount","unmount"],"description":"Virtual USB operation."},`+
			`"path":{"type":"string","minLength":1,"description":"Directory to list, relative to the volume root, for action=list. Default \"/\". No \"..\" components."},`+
			`"target":{"type":"string","enum":["Kvm","RemoteUsb"],"description":"Exposure target for action=mount. Kvm exposes the volume to the HDMI-controlled machine as a USB disk. Default Kvm."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":300000,"description":"RPC timeout in milliseconds. Default 30000."}`+
			`},"required":["action"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			action, ok := argString(args, "action")
			if !ok {
				return nil, fmt.Errorf("kvm_virtual_usb requires action")
			}
			path := "/"
			if value, has := argString(args, "path"); has {
				path = value
			}
			target := protocol.VirtualUsbKvm
			if value, has := argString(args, "target"); has {
				target = protocol.VirtualUsbTarget(value)
			}
			var timeoutMs int64
			if value, has := argInt(args, "timeoutMs"); has {
				timeoutMs = value
			}
			client, err := s.getClient("kvm_virtual_usb")
			if err != nil {
				return nil, err
			}
			switch action {
			case "state":
				state, err := client.GetVirtualUsbState(ctx)
				if err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "state": state})
			case "list":
				files, err := client.ListVirtualUsbFiles(ctx, path, timeoutMs)
				if err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "path": path, "files": files})
			case "mount":
				previous, err := client.GetVirtualUsbState(ctx)
				if err != nil {
					return nil, err
				}
				if previousRecord, has := previous.(map[string]any); has && previousRecord != nil {
					if currentTarget, ok := previousRecord["target"].(string); ok && currentTarget != "" && currentTarget != "None" {
						if err := client.UnmountVirtualUsbDrive(ctx, timeoutMs); err != nil {
							return nil, err
						}
					}
				}
				if err := client.MountVirtualUsbDrive(ctx, target, timeoutMs); err != nil {
					return nil, err
				}
				state, err := client.GetVirtualUsbState(ctx)
				if err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "mounted": true, "target": string(target), "state": state})
			case "unmount":
				previous, err := client.GetVirtualUsbState(ctx)
				if err != nil {
					return nil, err
				}
				if err := client.UnmountVirtualUsbDrive(ctx, timeoutMs); err != nil {
					return nil, err
				}
				return resultCallResult(map[string]any{"action": action, "unmounted": true, "previous": previous})
			}
			return nil, fmt.Errorf("kvm_virtual_usb requires action")
		})

	s.registerDeviceSettingsTool()
}

func (s *Server) registerStorageUploadTool() {
	register(s.mcp, "kvm_storage_upload",
		"Upload a controller-local file to the ArkKVM device over the WebRTC upload DataChannel. localPath is read on the machine running this MCP server.\n\n"+
			"DESTINATIONS\n"+
			"- storage (default) — device images folder for virtual media ISOs. Follow up with kvm_virtual_media action=mount_storage (OS-install flow); list with kvm_rpc listStorageFiles.\n"+
			"- fileTransfer — the virtual USB drive volume for ordinary files the controlled machine should see. Follow up with kvm_virtual_usb action=mount (when not mounted) and verify with kvm_virtual_usb action=list. remotePath selects the target directory (default '/').\n\n"+
			"PRECONDITIONS\n"+
			"- fileTransfer requires the volume to be mounted on the device: kvm_virtual_usb action=state reports the mount target, and when it is null run kvm_virtual_usb action=mount first. Note that action=unmount unmounts the filesystem on the device itself and blocks uploads and listings until it is mounted again.\n"+
			"- fileTransfer refuses an existing target file: pick another remoteFilename or delete the existing one with kvm_storage_delete.\n\n"+
			"TYPICAL ORDER (fileTransfer)\n"+
			"1. kvm_virtual_usb action=state — mount when the target is null.\n"+
			"2. kvm_storage_upload destination=fileTransfer localPath=... remotePath=/ remoteFilename=...\n"+
			"3. kvm_virtual_usb action=list path=<remotePath> — confirm the file is there.\n\n"+
			"RESUME\n"+
			"- Interrupted uploads keep partial data server-side; rerunning the same call resumes from the acknowledged byte offset (both destinations).\n\n"+
			"ERRORS (structuredContent carries ok=false with a stable code)\n"+
			"- FT_VOLUME_NOT_MOUNTED — fileTransfer volume unmounted → kvm_virtual_usb action=mount (or kvm_device_settings action=set_file_transfer).\n"+
			"- FT_FILE_EXISTS — target name already taken → change remoteFilename or delete the existing file with kvm_storage_delete.\n"+
			"- UPLOAD_INCOMPLETE — connection ended before the last byte → rerun the same call to resume.\n"+
			"- SOURCE_FILE_INVALID — localPath missing, not a regular file, empty, or over the size limit.\n"+
			"- DEVICE_NOT_CONFIGURED — no device URL yet → kvm_configure action=set.\n"+
			"- DEVICE_AUTH_REQUIRED — device enforces password auth and the credentials are missing, stale, or wrong (HTTP 401/403) → kvm_configure action=set password=..., or agentCookie + agentCsrfToken.\n\n"+
			"EXAMPLE\n"+
			"kvm_storage_upload destination=fileTransfer localPath=/tmp/report.pdf remotePath=/ remoteFilename=report.pdf",
		`{"type":"object","properties":{`+
			`"localPath":{"type":"string","minLength":1,"description":"Absolute or relative path of the file on the controller machine. Directories and special files are rejected."},`+
			`"destination":{"type":"string","enum":["storage","fileTransfer"],"description":"storage (default) = device images folder for virtual media ISOs (use with kvm_virtual_media); fileTransfer = the virtual USB drive volume for ordinary files (use with kvm_virtual_usb)."},`+
			`"remoteFilename":{"type":"string","minLength":1,"description":"Filename inside the destination. Defaults to the local file basename. No path separators."},`+
			`"remotePath":{"type":"string","minLength":1,"description":"Directory on the file-transfer volume for destination=fileTransfer, for example \"/\" or \"/docs\". Default \"/\". Ignored for destination=storage."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":3600000,"description":"Total upload budget in milliseconds. Default 1800000 (30 minutes)."}`+
			`},"required":["localPath"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				args, err := decodeArgs(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				localPath, ok := argString(args, "localPath")
				if !ok || localPath == "" {
					return nil, fmt.Errorf("kvm_storage_upload requires localPath")
				}
				remoteFilename, _ := argString(args, "remoteFilename")
				destination := "storage"
				if value, has := argString(args, "destination"); has {
					destination = value
				}
				remotePath, hasRemotePath := argString(args, "remotePath")
				var timeoutMs int64
				if value, has := argInt(args, "timeoutMs"); has {
					timeoutMs = value
				}
				client, err := s.getClient("kvm_storage_upload")
				if err != nil {
					return nil, err
				}
				options := device.UploadOptions{
					LocalPath:      localPath,
					RemoteFilename: remoteFilename,
					TimeoutMs:      timeoutMs,
				}
				if destination == "fileTransfer" {
					options.RemotePath = remotePath
					result, err := client.UploadFtFile(ctx, options)
					if err != nil {
						return nil, err
					}
					return mergeDestination(destination, map[string]any{
						"filename": result.Filename, "size": result.Size,
						"uploadedBytes": result.UploadedBytes, "resumedFromBytes": result.ResumedFromBytes,
						"complete": result.Complete, "durationMs": result.DurationMs, "uploadId": result.UploadID,
					}), nil
				}
				if hasRemotePath {
					return nil, fmt.Errorf("remotePath only applies when destination=fileTransfer")
				}
				result, err := client.UploadStorageFile(ctx, options)
				if err != nil {
					return nil, err
				}
				return mergeDestination(destination, map[string]any{
					"filename": result.Filename, "size": result.Size,
					"uploadedBytes": result.UploadedBytes, "resumedFromBytes": result.ResumedFromBytes,
					"complete": result.Complete, "durationMs": result.DurationMs, "uploadId": result.UploadID,
				}), nil
			})
		})
}

func (s *Server) registerStorageDeleteTool() {
	register(s.mcp, "kvm_storage_delete",
		"Delete one file from the ArkKVM device storage. This is destructive and always requires confirm=true. destination=storage removes an image from the virtual-media storage folder (as listed by kvm_rpc listStorageFiles); deleting a missing storage file is an idempotent success. destination=fileTransfer removes one regular file from the file-transfer volume (as listed by kvm_virtual_usb action=list) and requires that volume to be mounted. Directories are not deleted by this tool. A storage image that is currently mounted is rejected: unmount it with kvm_virtual_media action=unmount first, then delete it.",
		`{"type":"object","properties":{`+
			`"destination":{"type":"string","enum":["storage","fileTransfer"],"description":"storage (default) = virtual-media images folder; fileTransfer = virtual USB drive volume."},`+
			`"filename":{"type":"string","minLength":1,"description":"Storage filename to delete for destination=storage. No path separators."},`+
			`"path":{"type":"string","minLength":1,"description":"File path relative to the file-transfer volume root for destination=fileTransfer, for example \"/docs/report.pdf\". Must name a regular file, not a directory or the volume root."},`+
			`"confirm":{"type":"boolean","enum":[true],"description":"Must be explicitly true. Deletion cannot be undone."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"RPC timeout in milliseconds. Default 30000."}`+
			`},"required":["confirm"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				args, err := decodeArgs(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				confirm, hasConfirm := argBool(args, "confirm")
				if !hasConfirm || !confirm {
					return nil, fmt.Errorf("kvm_storage_delete requires confirm=true; deletion cannot be undone.")
				}
				destination := "storage"
				if value, has := argString(args, "destination"); has {
					destination = value
				}
				filename, hasFilename := argString(args, "filename")
				path, hasPath := argString(args, "path")
				var timeoutMs int64
				if value, has := argInt(args, "timeoutMs"); has {
					timeoutMs = value
				}
				switch destination {
				case "storage":
					if hasPath {
						return nil, fmt.Errorf("path only applies when destination=fileTransfer")
					}
					if !hasFilename || filename == "" {
						return nil, fmt.Errorf("kvm_storage_delete destination=storage requires filename")
					}
					safeFilename, err := protocol.SanitizeRemoteFilename(filename)
					if err != nil {
						return nil, err
					}
					client, err := s.getClient("kvm_storage_delete")
					if err != nil {
						return nil, err
					}
					current, err := client.GetVirtualMediaState(ctx)
					if err != nil {
						return nil, err
					}
					if record, ok := current.(map[string]any); ok && record != nil {
						source, _ := record["source"].(string)
						mounted, _ := record["filename"].(string)
						if source == string(protocol.SourceStorage) && mounted == safeFilename {
							return nil, fmt.Errorf("cannot delete mounted storage image %q; unmount it with kvm_virtual_media action=unmount first", safeFilename)
						}
					}
					if err := client.DeleteStorageFile(ctx, safeFilename, timeoutMs); err != nil {
						return nil, err
					}
					return map[string]any{"destination": destination, "deleted": true, "filename": safeFilename}, nil
				case "fileTransfer":
					if hasFilename {
						return nil, fmt.Errorf("filename only applies when destination=storage")
					}
					if !hasPath || path == "" {
						return nil, fmt.Errorf("kvm_storage_delete destination=fileTransfer requires path")
					}
					safePath, err := protocol.SanitizeFtFilePath(path)
					if err != nil {
						return nil, err
					}
					client, err := s.getClient("kvm_storage_delete")
					if err != nil {
						return nil, err
					}
					if err := client.DeleteVirtualUsbFile(ctx, safePath, timeoutMs); err != nil {
						return nil, err
					}
					return map[string]any{"destination": destination, "deleted": true, "path": safePath}, nil
				default:
					return nil, fmt.Errorf("unsupported destination for kvm_storage_delete: %s", destination)
				}
			})
		})
}

func mergeDestination(destination string, fields map[string]any) map[string]any {
	payload := map[string]any{"destination": destination}
	for key, value := range fields {
		payload[key] = value
	}
	return payload
}

func (s *Server) registerAtxPowerTool() {
	register(s.mcp, "kvm_atx_power",
		"Trigger an ATX power action on the HDMI-connected controlled machine through the ArkKVM ATX control lines: power-short behaves like tapping the power button, power-long like holding it (forced power-off), reset like pressing the reset button. Requires the ATX control extension to be wired to the target machine. This is the supported way to power-cycle the controlled machine so it boots from virtual media during an OS install. It is NOT the ArkKVM device reboot: rebooting the KVM itself would drop this session and unmount state, so that operation is intentionally not exposed here. Every call requires confirm=true.",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["power-short","power-long","reset"],"description":"ATX action sent to the controlled machine."},`+
			`"confirm":{"type":"boolean","enum":[true],"description":"Must be explicitly true. Guard against accidental power-off or reset of the controlled machine."}`+
			`},"required":["action","confirm"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			action, ok := argString(args, "action")
			if !ok {
				return nil, fmt.Errorf("kvm_atx_power requires action")
			}
			confirm, hasConfirm := argBool(args, "confirm")
			if !hasConfirm || !confirm {
				return nil, fmt.Errorf("kvm_atx_power requires confirm=true; the action controls the controlled machine physical power state.")
			}
			client, err := s.getClient("kvm_atx_power")
			if err != nil {
				return nil, err
			}
			if err := client.SetAtxPowerAction(ctx, protocol.AtxPowerAction(action), 0); err != nil {
				return nil, err
			}
			return resultCallResult(map[string]any{"sent": action, "confirm": true})
		})
}

func (s *Server) registerDeviceSettingsTool() {
	register(s.mcp, "kvm_device_settings",
		"Read or change the ArkKVM device hardware and USB-emulation settings shown on the web frontend Device settings page. This configures the KVM device itself, not the HDMI-connected computer.\n\n"+
			"ACTIONS\n"+
			"- get — no params. Returns guiSettings, usbDevices, usbConfig, and fileTransfer in one call.\n"+
			"- set_backlight — maxBrightness 0-255, dimAfterSeconds / offAfterSeconds 0-86400. At least one required; omitted fields keep their current value (the device replaces the whole backlight setting, so this reads then merges).\n"+
			"- set_display_rotation — rotation '90' (normal) or '270' (inverted).\n"+
			"- set_usb_devices — devices with keyboard, absolute_mouse, relative_mouse, mass_storage, microphone (all required), plus optional network_adapter {enable, adapter_type?, l3_config?}. mass_storage covers both the virtual-media and the file-transfer LUNs. Omitted network_adapter keeps the current device value.\n"+
			"- set_usb_config — usbConfig {vendor_id, product_id ('0x' + 4 hex digits), serial_number, manufacturer, product}: the USB identity the controlled machine sees.\n"+
			"- set_file_transfer — fileTransferEnabled boolean (the virtual USB drive switch).\n\n"+
			"PRECONDITIONS\n"+
			"- A device or cloud relay target must be configured first (kvm_configure action=set).\n\n"+
			"SIDE EFFECTS\n"+
			"- set_usb_devices, set_usb_config, and set_file_transfer reboot the USB gadget: the controlled machine re-enumerates its USB devices and HID input can drop for a moment — avoid while driving an installer.\n"+
			"- set_file_transfer to false unmounts the file-transfer volume from the remote USB port first.\n"+
			"- Every set_* action verifies by reading the state back and returns it.\n\n"+
			"ERRORS (structuredContent carries ok=false with a stable code)\n"+
			"- INVALID_ARGUMENT — required action field missing, or vendor_id/product_id not in 0x0000 form → fix the arguments.\n"+
			"- RPC_DEVICE_ERROR — device rejected the call; deviceCode/deviceData hold the cause.\n"+
			"- DEVICE_NOT_CONFIGURED — no device URL yet → kvm_configure action=set.\n"+
			"- DEVICE_AUTH_REQUIRED — device enforces password auth and the credentials are missing, stale, or wrong (HTTP 401/403) → kvm_configure action=set password=..., or agentCookie + agentCsrfToken.\n\n"+
			"EXAMPLES\n"+
			"kvm_device_settings action=get\n"+
			"kvm_device_settings action=set_backlight maxBrightness=35 dimAfterSeconds=600",
		`{"type":"object","properties":{`+
			`"action":{"type":"string","enum":["get","set_backlight","set_display_rotation","set_usb_devices","set_usb_config","set_file_transfer"],"description":"get reads all settings; the set_* actions apply one setting each and read the state back."},`+
			`"maxBrightness":{"type":"integer","minimum":0,"maximum":255,"description":"Backlight brightness for set_backlight. 0 turns the display off; the frontend presets are 0, 10, 35, and 64. Omitted fields keep their current value."},`+
			`"dimAfterSeconds":{"type":"integer","minimum":0,"maximum":86400,"description":"Seconds of inactivity before the display dims for set_backlight. 0 disables dimming; frontend presets are 0, 60, 300, 600, 1800, 3600. Omitted fields keep their current value."},`+
			`"offAfterSeconds":{"type":"integer","minimum":0,"maximum":86400,"description":"Seconds of inactivity before the display turns off for set_backlight. 0 disables power-off; frontend presets are 0, 300, 600, 1800, 3600. Omitted fields keep their current value."},`+
			`"rotation":{"type":"string","enum":["90","270"],"description":"Display orientation for set_display_rotation. 90 is normal, 270 is inverted."},`+
			`"devices":{"type":"object","properties":{`+
			`"keyboard":{"type":"boolean","description":"Keyboard emulation enabled."},`+
			`"absolute_mouse":{"type":"boolean","description":"Absolute-pointer mouse emulation enabled."},`+
			`"relative_mouse":{"type":"boolean","description":"Relative-pointer mouse emulation enabled."},`+
			`"mass_storage":{"type":"boolean","description":"Mass-storage emulation enabled. One legacy flag: the device applies it to both the virtual-media and the file-transfer mass-storage LUNs."},`+
			`"microphone":{"type":"boolean","description":"Microphone emulation enabled."},`+
			`"network_adapter":{"type":"object","properties":{`+
			`"enable":{"type":"boolean","description":"USB network adapter (NCM/RNDIS) enabled."},`+
			`"adapter_type":{"type":"string","enum":["ncm","rndis"],"description":"Gadget net function. Default ncm."},`+
			`"l3_config":{"type":"object","properties":{`+
			`"ip_address":{"type":"string","minLength":1,"description":"Board-side IP on the USB netdev (e.g. 172.16.0.1)."},`+
			`"subnet_mask":{"type":"string","minLength":1,"description":"Board-side subnet mask (e.g. 255.255.255.0)."},`+
			`"gateway":{"type":"string","minLength":1},`+
			`"dns_servers":{"type":"array","items":{"type":"string","minLength":1}}`+
			`},"required":["ip_address","subnet_mask"],"additionalProperties":false,"description":"Board L3 on usb0; omitted fields keep device defaults when enable flips on."}`+
			`},"required":["enable"],"additionalProperties":false,"description":"USB network adapter. Omitted: keep the device current value (MCP merges before setUsbDevices)."}`+
			`},"required":["keyboard","absolute_mouse","relative_mouse","mass_storage","microphone"],"additionalProperties":false,"description":"Emulated USB device classes for set_usb_devices. Five HID/storage/mic flags are required; network_adapter is optional and merged from current state when omitted. Reboots the USB gadget."},`+
			`"usbConfig":{"type":"object","properties":{`+
			`"vendor_id":{"type":"string","pattern":"^0x[0-9a-fA-F]{4}$","description":"USB vendor ID, for example 0x1d6b."},`+
			`"product_id":{"type":"string","pattern":"^0x[0-9a-fA-F]{4}$","description":"USB product ID, for example 0x0104."},`+
			`"serial_number":{"type":"string","minLength":1,"description":"USB serial number string reported to the controlled machine."},`+
			`"manufacturer":{"type":"string","minLength":1,"description":"USB manufacturer string reported to the controlled machine."},`+
			`"product":{"type":"string","minLength":1,"description":"USB product string reported to the controlled machine."}`+
			`},"required":["vendor_id","product_id","serial_number","manufacturer","product"],"additionalProperties":false,"description":"USB identifier strings for set_usb_config, as reported to the controlled machine. Reboots the USB gadget, so the target sees a new USB device."},`+
			`"fileTransferEnabled":{"type":"boolean","description":"File-transfer (virtual USB drive) switch for set_file_transfer. Reboots the mass-storage gadget; disabling it first unmounts the volume from the remote USB port."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"RPC timeout in milliseconds per call. Default 30000; setUsbDevices applies after a USB gadget restart and may need more."}`+
			`},"required":["action"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			return structured(func() (any, error) {
				args, err := decodeArgs(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				action, ok := argString(args, "action")
				if !ok {
					return nil, fmt.Errorf("kvm_device_settings requires action")
				}
				request := settings.Request{Action: action}
				if value, has := argNumber(args, "maxBrightness"); has {
					v := value
					request.MaxBrightness = &v
				}
				if value, has := argNumber(args, "dimAfterSeconds"); has {
					v := value
					request.DimAfterSeconds = &v
				}
				if value, has := argNumber(args, "offAfterSeconds"); has {
					v := value
					request.OffAfterSeconds = &v
				}
				if value, has := argString(args, "rotation"); has {
					request.Rotation = value
				}
				if devicesMap, has := argMap(args, "devices"); has {
					devices, err := parseUsbDevices(devicesMap)
					if err != nil {
						return nil, err
					}
					request.Devices = devices
				}
				if configMap, has := argMap(args, "usbConfig"); has {
					config, err := parseUsbConfig(configMap)
					if err != nil {
						return nil, err
					}
					request.UsbConfig = config
				}
				if value, has := argBool(args, "fileTransferEnabled"); has {
					request.FileTransferEnabled = &value
				}
				if value, has := argInt(args, "timeoutMs"); has {
					request.TimeoutMs = value
				}
				client, err := s.getClient("kvm_device_settings")
				if err != nil {
					return nil, err
				}
				return settings.ApplyDeviceSettingsAction(ctx, client, request)
			})
		})
}

func parseUsbDevices(map_ map[string]any) (*settings.UsbDevices, error) {
	devices := &settings.UsbDevices{}
	required := []struct {
		key string
		dst *bool
	}{
		{"keyboard", &devices.Keyboard},
		{"absolute_mouse", &devices.AbsoluteMouse},
		{"relative_mouse", &devices.RelativeMouse},
		{"mass_storage", &devices.MassStorage},
		{"microphone", &devices.Microphone},
	}
	for _, field := range required {
		value, ok := argBool(map_, field.key)
		if !ok {
			return nil, fmt.Errorf("set_usb_devices requires devices.%s (boolean)", field.key)
		}
		*field.dst = value
	}
	if adapterMap, ok := argMap(map_, "network_adapter"); ok {
		adapter := &settings.NetworkAdapter{}
		enable, ok := argBool(adapterMap, "enable")
		if !ok {
			return nil, fmt.Errorf("devices.network_adapter requires enable (boolean)")
		}
		adapter.Enable = enable
		if adapterType, has := argString(adapterMap, "adapter_type"); has {
			adapter.AdapterType = adapterType
		}
		if l3, has := argMap(adapterMap, "l3_config"); has {
			l3Config := &settings.L3Config{}
			ip, okIP := argString(l3, "ip_address")
			mask, okMask := argString(l3, "subnet_mask")
			if !okIP || ip == "" || !okMask || mask == "" {
				return nil, fmt.Errorf("devices.network_adapter.l3_config requires ip_address and subnet_mask")
			}
			l3Config.IPAddress = ip
			l3Config.SubnetMask = mask
			if gateway, has := argString(l3, "gateway"); has {
				l3Config.Gateway = gateway
			}
			if dns, has := argArray(l3, "dns_servers"); has {
				for _, entry := range dns {
					if s, ok := entry.(string); ok {
						l3Config.DNSServers = append(l3Config.DNSServers, s)
					}
				}
			}
			adapter.L3Config = l3Config
		}
		devices.NetworkAdapter = adapter
	}
	return devices, nil
}

func parseUsbConfig(map_ map[string]any) (*settings.UsbConfig, error) {
	config := &settings.UsbConfig{}
	for _, field := range []struct {
		key string
		dst *string
	}{
		{"vendor_id", &config.VendorID},
		{"product_id", &config.ProductID},
		{"serial_number", &config.SerialNumber},
		{"manufacturer", &config.Manufacturer},
		{"product", &config.Product},
	} {
		value, ok := argString(map_, field.key)
		if !ok || value == "" {
			return nil, fmt.Errorf("set_usb_config requires usbConfig.%s", field.key)
		}
		*field.dst = value
	}
	return config, nil
}
