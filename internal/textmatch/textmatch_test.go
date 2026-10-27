package textmatch

import "testing"

func TestNormalizeScreenText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello   world", "hello world"},
		{"  leading and trailing  ", "leading and trailing"},
		{"a\n\tb\u00a0 c", "a b c"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := NormalizeScreenText(c.in); got != c.want {
			t.Errorf("NormalizeScreenText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScreenTextIncludes(t *testing.T) {
	haystack := "Setup Complete\n\nWelcome   to the installer"
	if !ScreenTextIncludes(haystack, "setup complete", false) {
		t.Error("case-insensitive match across line breaks failed")
	}
	if !ScreenTextIncludes(haystack, "Setup Complete", true) {
		t.Error("case-sensitive exact match failed")
	}
	if ScreenTextIncludes(haystack, "setup complete", true) {
		t.Error("case-sensitive match must not ignore case")
	}
	if ScreenTextIncludes(haystack, "", false) {
		t.Error("empty needle must never match")
	}
	if ScreenTextIncludes(haystack, "   ", false) {
		t.Error("whitespace-only needle normalizes to empty and must never match")
	}
	if !ScreenTextIncludes(haystack, "welcome to", false) {
		t.Error("multi-space haystack must collapse before matching")
	}
}
