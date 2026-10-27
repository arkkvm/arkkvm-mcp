package hid

import "testing"

func TestResolveModifierToken(t *testing.T) {
	cases := map[string]ModifierName{
		"ctrl": ModCtrl, "control": ModCtrl, "CTRL": ModCtrl,
		"shift": ModShift,
		"alt":   ModAlt, "option": ModAlt, "altgr": ModAlt,
		"gui": ModGui, "win": ModGui, "windows": ModGui, "cmd": ModGui,
		"command": ModGui, "super": ModGui, "meta": ModGui,
	}
	for token, want := range cases {
		got, err := ResolveModifierToken(token)
		if err != nil || got != want {
			t.Errorf("ResolveModifierToken(%q) = %v, %v; want %v", token, got, err, want)
		}
	}
	if _, err := ResolveModifierToken("hyper"); err == nil {
		t.Error("unknown modifier must fail")
	}
}

func TestPrepareStepBasic(t *testing.T) {
	prepared, err := PrepareStep(RawStep{Keys: []string{"delete"}, Modifiers: []string{"ctrl", "alt"}}, StepDefaults{HoldMs: 60, DelayAfterMs: 60})
	if err != nil {
		t.Fatalf("PrepareStep: %v", err)
	}
	// delete = 0x4c = 76
	wantKeys := []int{0x4c, 0, 0, 0, 0, 0}
	for i, v := range wantKeys {
		if prepared.Press.Keys[i] != v {
			t.Errorf("press.Keys[%d] = %d, want %d", i, prepared.Press.Keys[i], v)
		}
	}
	if prepared.Press.Modifier != 0x05 {
		t.Errorf("press.Modifier = %#x, want 0x05", prepared.Press.Modifier)
	}
	if len(prepared.Release.Keys) != 6 || prepared.Release.Modifier != 0 {
		t.Errorf("release must be the all-zero report, got %+v", prepared.Release)
	}
	if prepared.Step.HoldMs != 60 || prepared.Step.DelayAfterMs != 60 {
		t.Errorf("defaults not applied: %+v", prepared.Step)
	}
}

func TestPrepareStepAliasesAndDuplicates(t *testing.T) {
	prepared, err := PrepareStep(RawStep{Keys: []string{"Return", " ESC "}}, StepDefaults{HoldMs: 0, DelayAfterMs: 0})
	if err != nil {
		t.Fatalf("PrepareStep: %v", err)
	}
	if prepared.Step.Keys[0] != "return" || prepared.Step.Keys[1] != "esc" {
		t.Errorf("tokens must be trimmed and lowercased: %v", prepared.Step.Keys)
	}
	if prepared.Press.Keys[0] != 0x28 || prepared.Press.Keys[1] != 0x29 {
		t.Errorf("alias resolution failed: %v", prepared.Press.Keys)
	}
	// Same usage id through an alias = duplicate.
	if _, err := PrepareStep(RawStep{Keys: []string{"enter", "return"}}, StepDefaults{HoldMs: 0, DelayAfterMs: 0}); err == nil {
		t.Error("duplicate usage ids in one step must fail")
	}
}

func TestPrepareStepValidation(t *testing.T) {
	if _, err := PrepareStep(RawStep{Keys: nil}, StepDefaults{HoldMs: 0, DelayAfterMs: 0}); err == nil {
		t.Error("empty step must fail")
	}
	seven := make([]string, 7)
	for i := range seven {
		seven[i] = "a"
	}
	if _, err := PrepareStep(RawStep{Keys: seven}, StepDefaults{HoldMs: 0, DelayAfterMs: 0}); err == nil {
		t.Error("7 keys must fail")
	}
	if _, err := PrepareStep(RawStep{Keys: []string{"f13"}}, StepDefaults{HoldMs: 0, DelayAfterMs: 0}); err == nil {
		t.Error("unknown key must fail")
	}
	hold := -1
	if _, err := PrepareStep(RawStep{Keys: []string{"a"}, HoldMs: &hold}, StepDefaults{HoldMs: 0, DelayAfterMs: 0}); err == nil {
		t.Error("negative holdMs must fail")
	}
	hold = 5001
	if _, err := PrepareStep(RawStep{Keys: []string{"a"}, HoldMs: &hold}, StepDefaults{HoldMs: 0, DelayAfterMs: 0}); err == nil {
		t.Error("holdMs > 5000 must fail")
	}
}

func TestPrepareStepDuplicateModifiers(t *testing.T) {
	prepared, err := PrepareStep(
		RawStep{Keys: []string{"a"}, Modifiers: []string{"ctrl", "control"}},
		StepDefaults{HoldMs: 0, DelayAfterMs: 0},
	)
	if err != nil {
		t.Fatalf("PrepareStep: %v", err)
	}
	if len(prepared.Step.Modifiers) != 1 || prepared.Press.Modifier != 0x01 {
		t.Errorf("duplicate modifiers must collapse: %+v", prepared.Step.Modifiers)
	}
}
