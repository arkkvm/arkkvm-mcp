package ocr

import "testing"

func TestCleanOcrText(t *testing.T) {
	input := "  Setup   Complete \r\n\r\nWelcome\tto  the\tinstaller \n\n"
	want := "Setup Complete\nWelcome to the installer"
	if got := CleanOcrText(input); got != want {
		t.Errorf("CleanOcrText = %q, want %q", got, want)
	}
	if CleanOcrText("") != "" {
		t.Error("empty input must stay empty")
	}
}

func TestRecognizePNGRejectsNonPNG(t *testing.T) {
	if _, err := RecognizePNG([]byte("JUNKJUNK"), Options{}); err == nil {
		t.Error("non-PNG input must fail with the exact message")
	} else if err.Error() != "HDMI OCR input is not a PNG image" {
		t.Errorf("unexpected error: %v", err)
	}
}
