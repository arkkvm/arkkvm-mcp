// Package textmatch provides literal text matching for hdmi_wait_for_text.
// OCR output contains arbitrary line breaks and spacing, so both the haystack
// and the needle are whitespace-normalized before a plain substring check.
// Deliberately no regex support: an unbounded caller-supplied pattern is a
// ReDoS vector and literal matching covers the installer-wizard use case.
package textmatch

import "strings"

// jsWhitespace reports whether r is in the JavaScript `\s` character class,
// which is wider than Go's `\s` (it includes NBSP, ideographic spaces, and
// line/paragraph separators, all common in OCR output).
func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// NormalizeScreenText collapses every whitespace run to a single space and
// trims the ends.
func NormalizeScreenText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	pendingSpace := false
	for _, r := range text {
		if jsWhitespace(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// ScreenTextIncludes reports whether the normalized haystack contains the
// normalized needle. An empty needle never matches.
func ScreenTextIncludes(haystack, needle string, caseSensitive bool) bool {
	normalizedHaystack := NormalizeScreenText(haystack)
	normalizedNeedle := NormalizeScreenText(needle)
	if normalizedNeedle == "" {
		return false
	}
	if caseSensitive {
		return strings.Contains(normalizedHaystack, normalizedNeedle)
	}
	return strings.Contains(strings.ToLower(normalizedHaystack), strings.ToLower(normalizedNeedle))
}
