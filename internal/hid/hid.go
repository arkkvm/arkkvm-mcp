// Package hid holds the USB HID keyboard usage table and key-sequence
// planning for the kvm_key_sequence tool. Keeping the mapping here (instead
// of inside the tool handler) makes it testable without a device connection.
//
// The target machine interprets reports through its own OS keyboard layout;
// the usage IDs below follow the USB HID usage table (HID Usage Tables 1.5,
// Keyboard/Keypad page 0x07). Layout-dependent legends may differ, but the
// usages resolve to the same physical key as the browser client's virtual
// keyboard.
package hid

import (
	"fmt"
	"strings"
)

// ModifierName is a canonical modifier identifier.
type ModifierName string

const (
	ModCtrl  ModifierName = "ctrl"
	ModShift ModifierName = "shift"
	ModAlt   ModifierName = "alt"
	ModGui   ModifierName = "gui"
)

// ModifierBits maps canonical modifier names to their USB HID byte bits.
var ModifierBits = map[ModifierName]byte{
	ModCtrl:  0x01,
	ModShift: 0x02,
	ModAlt:   0x04,
	ModGui:   0x08,
}

var modifierAliases = map[string]ModifierName{
	"ctrl":    ModCtrl,
	"control": ModCtrl,
	"shift":   ModShift,
	"alt":     ModAlt,
	"option":  ModAlt,
	"altgr":   ModAlt,
	"gui":     ModGui,
	"win":     ModGui,
	"windows": ModGui,
	"cmd":     ModGui,
	"command": ModGui,
	"super":   ModGui,
	"meta":    ModGui,
}

// ResolveModifierToken maps one user-supplied modifier token to its canonical
// name, with the same aliases as the web client.
func ResolveModifierToken(token string) (ModifierName, error) {
	canonical, ok := modifierAliases[strings.ToLower(strings.TrimSpace(token))]
	if !ok {
		return "", fmt.Errorf(
			"Unknown keyboard modifier %q. Valid names: ctrl, shift, alt, gui (aliases: control, option, win, windows, cmd, command, super, meta).",
			token,
		)
	}
	return canonical, nil
}

// KeyUsage is the canonical key token -> USB HID usage ID table.
var KeyUsage = map[string]byte{
	"a": 0x04, "b": 0x05, "c": 0x06, "d": 0x07, "e": 0x08, "f": 0x09, "g": 0x0a, "h": 0x0b,
	"i": 0x0c, "j": 0x0d, "k": 0x0e, "l": 0x0f, "m": 0x10, "n": 0x11, "o": 0x12, "p": 0x13,
	"q": 0x14, "r": 0x15, "s": 0x16, "t": 0x17, "u": 0x18, "v": 0x19, "w": 0x1a, "x": 0x1b,
	"y": 0x1c, "z": 0x1d,
	"1": 0x1e, "2": 0x1f, "3": 0x20, "4": 0x21, "5": 0x22,
	"6": 0x23, "7": 0x24, "8": 0x25, "9": 0x26, "0": 0x27,
	"enter": 0x28, "return": 0x28,
	"esc": 0x29, "escape": 0x29,
	"backspace": 0x2a,
	"tab":       0x2b,
	"space":     0x2c,
	"minus":     0x2d, "equal": 0x2e,
	"leftbracket": 0x2f, "rightbracket": 0x30, "backslash": 0x31,
	"semicolon": 0x33, "quote": 0x34, "backquote": 0x35,
	"comma": 0x36, "dot": 0x37, "slash": 0x38,
	"capslock": 0x39,
	"f1":       0x3a, "f2": 0x3b, "f3": 0x3c, "f4": 0x3d, "f5": 0x3e, "f6": 0x3f,
	"f7": 0x40, "f8": 0x41, "f9": 0x42, "f10": 0x43, "f11": 0x44, "f12": 0x45,
	"printscreen": 0x46, "scrolllock": 0x47, "pause": 0x48,
	"insert": 0x49, "home": 0x4a, "pageup": 0x4b,
	"delete": 0x4c, "end": 0x4d, "pagedown": 0x4e,
	"right": 0x4f, "left": 0x50, "down": 0x51, "up": 0x52,
	"numlock": 0x53,
}

// MaxKeysPerStep is the USB HID boot-protocol rollover limit.
const MaxKeysPerStep = 6

// RawStep is one unvalidated step from the kvm_key_sequence caller.
type RawStep struct {
	Keys         []string
	Modifiers    []string
	HoldMs       *int
	DelayAfterMs *int
}

// StepDefaults are the tool-level hold/delay defaults.
type StepDefaults struct {
	HoldMs       int
	DelayAfterMs int
}

// Step is one validated key sequence step.
type Step struct {
	// Keys holds the resolved key tokens in send order.
	Keys []string
	// Modifiers holds the canonical modifier names held during this step.
	Modifiers    []ModifierName
	HoldMs       int
	DelayAfterMs int
}

// Report is a raw USB HID keyboard report. Keys is []int (not []byte)
// because encoding/json would base64-encode a byte slice, while the device
// expects a JSON number array.
type Report struct {
	Keys     []int `json:"keys"`
	Modifier int   `json:"modifier"`
}

// PreparedStep pairs the key-down and key-up reports for one step.
type PreparedStep struct {
	Step    Step
	Press   Report
	Release Report
}

// PrepareStep validates and resolves one raw step into a key-down/key-up HID
// report pair, with an actionable error for unknown tokens, duplicate keys
// inside one step, or more than six keys per step.
func PrepareStep(raw RawStep, defaults StepDefaults) (PreparedStep, error) {
	if len(raw.Keys) == 0 {
		return PreparedStep{}, fmt.Errorf("Each keyboard step requires at least one key")
	}
	if len(raw.Keys) > MaxKeysPerStep {
		return PreparedStep{}, fmt.Errorf("A keyboard step accepts at most %d keys, received %d", MaxKeysPerStep, len(raw.Keys))
	}
	usageIDs := make([]byte, 0, len(raw.Keys))
	for _, token := range raw.Keys {
		normalized := strings.ToLower(strings.TrimSpace(token))
		usage, ok := KeyUsage[normalized]
		if !ok {
			return PreparedStep{}, fmt.Errorf(
				"Unknown keyboard key %q. Use USB HID keyboard tokens such as enter, esc, tab, space, delete, up, down, left, right, f1..f12, a..z, 0..9, or use kvm_keyboard for raw usage IDs.",
				token,
			)
		}
		for _, existing := range usageIDs {
			if existing == usage {
				return PreparedStep{}, fmt.Errorf("Duplicate key %q in one keyboard step", token)
			}
		}
		usageIDs = append(usageIDs, usage)
	}
	var modifier int
	modifiers := make([]ModifierName, 0, len(raw.Modifiers))
	for _, token := range raw.Modifiers {
		canonical, err := ResolveModifierToken(token)
		if err != nil {
			return PreparedStep{}, err
		}
		seen := false
		for _, existing := range modifiers {
			if existing == canonical {
				seen = true
				break
			}
		}
		if !seen {
			modifiers = append(modifiers, canonical)
			modifier |= int(ModifierBits[canonical])
		}
	}
	holdMs := defaults.HoldMs
	if raw.HoldMs != nil {
		holdMs = *raw.HoldMs
	}
	delayAfterMs := defaults.DelayAfterMs
	if raw.DelayAfterMs != nil {
		delayAfterMs = *raw.DelayAfterMs
	}
	if holdMs < 0 || holdMs > 5_000 {
		return PreparedStep{}, fmt.Errorf("Keyboard step holdMs must be between 0 and 5000")
	}
	if delayAfterMs < 0 || delayAfterMs > 5_000 {
		return PreparedStep{}, fmt.Errorf("Keyboard step delayAfterMs must be between 0 and 5000")
	}
	padded := make([]int, MaxKeysPerStep)
	for i, u := range usageIDs {
		padded[i] = int(u)
	}
	keys := make([]string, len(raw.Keys))
	for i, k := range raw.Keys {
		keys[i] = strings.ToLower(strings.TrimSpace(k))
	}
	return PreparedStep{
		Step:    Step{Keys: keys, Modifiers: modifiers, HoldMs: holdMs, DelayAfterMs: delayAfterMs},
		Press:   Report{Keys: padded, Modifier: modifier},
		Release: Report{Keys: []int{0, 0, 0, 0, 0, 0}, Modifier: 0},
	}, nil
}
