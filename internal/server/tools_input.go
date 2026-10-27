package server

import (
	"context"
	"fmt"
	"time"

	"arkkvm-mcp/internal/hid"
)

func (s *Server) registerInputTools() {
	register(s.mcp, "kvm_keyboard",
		// TODO: Detect the controller platform (macOS, Windows, or Linux) and expose platform-specific shortcut mappings in this API description.
		"Send one USB HID keyboard report to the HDMI-connected controlled machine through the ArkKVM USB connection. This accepts HID usage IDs, not text, Unicode characters, browser key names, or macOS key codes. The target machine interprets the report according to its own OS and keyboard layout. keys contains up to six simultaneously pressed non-modifier HID usage IDs; modifier is an 8-bit bitmask. For a normal key press send a key-down report, wait briefly if needed, then send a release report with keys all zero and modifier zero. For a shortcut, send the modifier and key together, then always release all keys. Common modifier bits: Left Ctrl=0x01, Left Shift=0x02, Left Alt=0x04, Left GUI/Windows/Command=0x08, Right Ctrl=0x10, Right Shift=0x20, Right Alt=0x40, Right GUI=0x80. On a Windows target, use Left GUI (0x08) for the Windows key even when the controller runs macOS; the controller platform must not be substituted for the target platform. Platform-specific shortcut mapping is not implemented yet.",
		`{"type":"object","properties":{`+
			`"keys":{"type":"array","items":{"type":"integer","minimum":0,"maximum":255},"maxItems":6,"description":"Up to six USB HID usage IDs for simultaneously pressed non-modifier keys. Example: Keyboard A is 0x04; Enter is 0x28; the Windows/GUI key is a modifier and belongs in modifier, not keys. Use [] or [0, 0, 0, 0, 0, 0] for release."},`+
			`"modifier":{"type":"integer","minimum":0,"maximum":255,"description":"USB HID modifier bitmask. Combine bits with bitwise OR: Left Ctrl 0x01, Left Shift 0x02, Left Alt 0x04, Left GUI/Windows/Command 0x08, Right Ctrl 0x10, Right Shift 0x20, Right Alt/AltGr 0x40, Right GUI 0x80. Use 0 when no modifier is pressed."}`+
			`},"required":["keys","modifier"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			keys, ok := argArray(args, "keys")
			if !ok {
				return nil, fmt.Errorf("kvm_keyboard requires keys")
			}
			if len(keys) > 6 {
				return nil, fmt.Errorf("keys accepts at most 6 usage IDs")
			}
			keyList := make([]int, 0, len(keys))
			for _, key := range keys {
				f, ok := key.(float64)
				if !ok || f < 0 || f > 255 || f != float64(int64(f)) {
					return nil, fmt.Errorf("keys entries must be integers in 0..255")
				}
				keyList = append(keyList, int(f))
			}
			modifier, ok := argNumber(args, "modifier")
			if !ok || modifier < 0 || modifier > 255 || modifier != float64(int64(modifier)) {
				return nil, fmt.Errorf("kvm_keyboard requires modifier (integer 0..255)")
			}
			client, err := s.getClient("kvm_keyboard")
			if err != nil {
				return nil, err
			}
			if err := client.Input(ctx, "keyboardReport", map[string]any{"keys": keyList, "modifier": int(modifier)}); err != nil {
				return nil, err
			}
			return resultCallResult(map[string]any{"success": true, "channel": "input", "method": "keyboardReport"})
		})

	register(s.mcp, "kvm_mouse",
		"Send one USB HID mouse report to the HDMI-connected controlled machine through ArkKVM. Use absolute mode when the screen coordinate is known or when first placing the pointer; use relative mode for small movement or dragging. Absolute coordinates use the HID range 0..32767 in both axes, where (0,0) is the top-left and (16384,16384) is approximately the center. buttons is a held-button bitmask: Left=1, Right=2, Middle=4. A click requires separate reports with the same position: buttons=1, then buttons=0. Always release buttons after a click or drag. This tool moves/clicks the target pointer; it does not provide screen content or verify that the pointer landed on a visible control. For scrolling, first move to the scrollable target and click if focus is needed, then call kvm_wheel.",
		`{"type":"object","properties":{`+
			`"mode":{"type":"string","enum":["relative","absolute"],"description":"Input mode. Use absolute for screen placement/clicking; use relative for movement by dx/dy or dragging."},`+
			`"x":{"type":"integer","minimum":0,"maximum":32767,"description":"Absolute horizontal HID coordinate, 0..32767 from left to right. Required when mode is absolute; ignored in relative mode."},`+
			`"y":{"type":"integer","minimum":0,"maximum":32767,"description":"Absolute vertical HID coordinate, 0..32767 from top to bottom. Required when mode is absolute; ignored in relative mode."},`+
			`"dx":{"type":"integer","minimum":-128,"maximum":127,"description":"Relative horizontal movement for one HID report, -128..127. Required when mode is relative; ignored in absolute mode."},`+
			`"dy":{"type":"integer","minimum":-128,"maximum":127,"description":"Relative vertical movement for one HID report, -128..127. Required when mode is relative; ignored in absolute mode."},`+
			`"buttons":{"type":"integer","minimum":0,"maximum":255,"description":"Held mouse-button bitmask: Left=1, Right=2, Middle=4. Use 0 for movement without a held button and for button release."}`+
			`},"required":["mode","buttons"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			mode, ok := argString(args, "mode")
			if !ok {
				return nil, fmt.Errorf("kvm_mouse requires mode (relative/absolute)")
			}
			buttons, hasButtons := argInt(args, "buttons")
			if !hasButtons || buttons < 0 || buttons > 255 {
				return nil, fmt.Errorf("kvm_mouse requires buttons (integer 0..255)")
			}
			client, err := s.getClient("kvm_mouse")
			if err != nil {
				return nil, err
			}
			var inputErr error
			if mode == "absolute" {
				x, hasX := argInt(args, "x")
				y, hasY := argInt(args, "y")
				if !hasX || !hasY {
					return nil, fmt.Errorf("absolute mouse requires x and y")
				}
				inputErr = client.Input(ctx, "absMouseReport", map[string]any{"x": x, "y": y, "buttons": buttons})
			} else {
				dx, hasDX := argInt(args, "dx")
				dy, hasDY := argInt(args, "dy")
				if !hasDX || !hasDY {
					return nil, fmt.Errorf("relative mouse requires dx and dy")
				}
				inputErr = client.Input(ctx, "relMouseReport", map[string]any{"dx": dx, "dy": dy, "buttons": buttons})
			}
			if inputErr != nil {
				return nil, inputErr
			}
			return resultCallResult(map[string]any{"success": true, "channel": "input", "mode": mode})
		})

	register(s.mcp, "kvm_wheel",
		"Send one USB HID mouse-wheel report through ArkKVM to the controlled machine. The wheel acts at the target machine current pointer position, so move the pointer over a scrollable control first; if the application needs focus, click at that position and release the button before scrolling. wheelY is a signed HID tick, normally -1 for one notch down and +1 for one notch up. Repeat this tool call for a larger scroll; do not pass browser pixel deltas such as 100 as one report. wheelX is optional horizontal scrolling and defaults to 0. The result confirms that the report was sent, not that a particular page visibly scrolled.",
		`{"type":"object","properties":{`+
			`"wheelY":{"type":"integer","minimum":-128,"maximum":127,"description":"Vertical wheel movement in signed HID ticks. Use -1 for one notch down, +1 for one notch up; repeat calls for more scrolling. Use 0 when only horizontal scrolling is required."},`+
			`"wheelX":{"type":"integer","minimum":-128,"maximum":127,"description":"Optional horizontal wheel movement in signed HID ticks. Defaults to 0."}`+
			`},"required":["wheelY"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			wheelY, ok := argInt(args, "wheelY")
			if !ok || wheelY < -128 || wheelY > 127 {
				return nil, fmt.Errorf("kvm_wheel requires wheelY (integer -128..127)")
			}
			report := map[string]any{"wheelY": wheelY}
			if wheelX, hasX := argInt(args, "wheelX"); hasX {
				report["wheelX"] = wheelX
			}
			client, err := s.getClient("kvm_wheel")
			if err != nil {
				return nil, err
			}
			if err := client.Input(ctx, "wheelReport", report); err != nil {
				return nil, err
			}
			return resultCallResult(map[string]any{"success": true, "channel": "input", "method": "wheelReport"})
		})

	register(s.mcp, "kvm_key_sequence",
		"Send a sequence of USB HID keyboard chords to the HDMI-connected controlled machine through ArkKVM, using named keys instead of raw usage IDs. Each step holds one key combination (up to six keys plus modifiers), sends the key-down report, waits holdMs, and always sends the release report before continuing. This is the preferred typing tool for installer wizards: use steps like [{keys:[\"enter\"]}], [{keys:[\"d\",\"e\",\"l\"],modifiers:[]}], or one chord step [{keys:[\"delete\"],modifiers:[\"ctrl\",\"alt\"]}]. Key tokens are interpreted by the target machine own OS keyboard layout. For raw HID usage IDs use kvm_keyboard instead.",
		`{"type":"object","properties":{`+
			`"steps":{"type":"array","minItems":1,"maxItems":64,"items":{"type":"object","properties":{`+
			`"keys":{"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":6,"description":"Named keys pressed together in this step. Tokens: a-z, 0-9, enter, esc, backspace, tab, space, minus, equal, leftbracket, rightbracket, backslash, semicolon, quote, backquote, comma, dot, slash, capslock, f1..f12, printscreen, scrolllock, pause, insert, home, pageup, delete, end, pagedown, right, left, down, up, numlock."},`+
			`"modifiers":{"type":"array","items":{"type":"string","minLength":1},"description":"Modifier names held during this step: ctrl, shift, alt, gui. Aliases: control, option, win, windows, cmd, command, super, meta. On a Windows target the Windows key is gui."},`+
			`"holdMs":{"type":"integer","minimum":0,"maximum":5000,"description":"How long this step keys are held before the release report. Overrides the tool-level holdMs for this step."},`+
			`"delayAfterMs":{"type":"integer","minimum":0,"maximum":5000,"description":"Extra pause after this step release before the next step. Overrides the tool-level delayAfterMs for this step."}`+
			`},"required":["keys"],"additionalProperties":false},"description":"Ordered keyboard steps. One entry per key press or chord; sequences of entries type one key after another."},`+
			`"holdMs":{"type":"integer","minimum":0,"maximum":1000,"description":"Default milliseconds each step keys are held before release. Default 60."},`+
			`"delayAfterMs":{"type":"integer","minimum":0,"maximum":1000,"description":"Default pause between steps after the release report. Default 60."}`+
			`},"required":["steps"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			rawSteps, ok := argArray(args, "steps")
			if !ok || len(rawSteps) == 0 {
				return nil, fmt.Errorf("kvm_key_sequence requires steps")
			}
			holdMs := int64(60)
			if value, has := argInt(args, "holdMs"); has {
				holdMs = value
			}
			delayAfterMs := int64(60)
			if value, has := argInt(args, "delayAfterMs"); has {
				delayAfterMs = value
			}
			defaults := hid.StepDefaults{HoldMs: int(holdMs), DelayAfterMs: int(delayAfterMs)}
			prepared := make([]hid.PreparedStep, 0, len(rawSteps))
			for _, rawStep := range rawSteps {
				stepMap, ok := rawStep.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("Each keyboard step requires at least one key")
				}
				raw := hid.RawStep{}
				if keyTokens, has := argArray(stepMap, "keys"); has {
					raw.Keys = make([]string, 0, len(keyTokens))
					for _, token := range keyTokens {
						s, ok := token.(string)
						if !ok {
							return nil, fmt.Errorf("Each keyboard step requires at least one key")
						}
						raw.Keys = append(raw.Keys, s)
					}
				}
				if modifierTokens, has := argArray(stepMap, "modifiers"); has {
					raw.Modifiers = make([]string, 0, len(modifierTokens))
					for _, token := range modifierTokens {
						if s, ok := token.(string); ok {
							raw.Modifiers = append(raw.Modifiers, s)
						}
					}
				}
				if hold, has := argInt(stepMap, "holdMs"); has {
					value := int(hold)
					raw.HoldMs = &value
				}
				if delay, has := argInt(stepMap, "delayAfterMs"); has {
					value := int(delay)
					raw.DelayAfterMs = &value
				}
				preparedStep, err := hid.PrepareStep(raw, defaults)
				if err != nil {
					return nil, err
				}
				prepared = append(prepared, preparedStep)
			}
			client, err := s.getClient("kvm_key_sequence")
			if err != nil {
				return nil, err
			}
			sentSteps := 0
			for _, entry := range prepared {
				if sendErr := client.Input(ctx, "keyboardReport", map[string]any{"keys": entry.Press.Keys, "modifier": entry.Press.Modifier}); sendErr != nil {
					s.releaseAllKeys(ctx, client)
					return nil, sendErr
				}
				time.Sleep(time.Duration(entry.Step.HoldMs) * time.Millisecond)
				if sendErr := client.Input(ctx, "keyboardReport", map[string]any{"keys": entry.Release.Keys, "modifier": entry.Release.Modifier}); sendErr != nil {
					s.releaseAllKeys(ctx, client)
					return nil, sendErr
				}
				time.Sleep(time.Duration(entry.Step.DelayAfterMs) * time.Millisecond)
				sentSteps++
			}
			return resultCallResult(map[string]any{"sent": sentSteps, "total": len(prepared), "released": true, "channel": "input"})
		})
}

func (s *Server) releaseAllKeys(ctx context.Context, client interface {
	Input(ctx context.Context, method string, params any) error
}) {
	// Best-effort release when the input channel already failed.
	_ = client.Input(ctx, "keyboardReport", map[string]any{"keys": []int{0, 0, 0, 0, 0, 0}, "modifier": 0})
}
