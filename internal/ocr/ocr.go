// Package ocr wraps the local tesseract command for HDMI screen text
// recognition. Each call writes the PNG to a temp directory that is always
// cleaned up afterwards.
package ocr

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	defaultLanguage = "eng+chi_sim"
	defaultPSM      = 6
	maxOutputBytes  = 1024 * 1024
)

// Options tune one recognition call.
type Options struct {
	Language string
	PSM      *int
}

// Result is the recognized screen text.
type Result struct {
	Text     string `json:"text"`
	Language string `json:"language"`
	PSM      int    `json:"psm"`
}

var pngMagic = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// RecognizePNG runs OCR over one PNG image.
func RecognizePNG(image []byte, opts Options) (Result, error) {
	if len(image) < 8 || !bytes.HasPrefix(image, pngMagic) {
		return Result{}, fmt.Errorf("HDMI OCR input is not a PNG image")
	}
	language := opts.Language
	if language == "" {
		language = os.Getenv("ARKKVM_OCR_LANG")
	}
	if language == "" {
		language = defaultLanguage
	}
	psm := defaultPSM
	if opts.PSM != nil {
		psm = *opts.PSM
	}
	if strings.TrimSpace(language) == "" {
		return Result{}, fmt.Errorf("OCR language must not be empty")
	}
	if psm < 0 || psm > 13 {
		return Result{}, fmt.Errorf("OCR page segmentation mode must be between 0 and 13")
	}

	directory, err := os.MkdirTemp("", "arkkvm-ocr-")
	if err != nil {
		return Result{}, err
	}
	defer func() {
		_ = os.RemoveAll(directory)
	}()
	inputPath := filepath.Join(directory, "frame.png")
	if err := os.WriteFile(inputPath, image, 0o600); err != nil {
		return Result{}, err
	}
	command := os.Getenv("ARKKVM_OCR_COMMAND")
	if command == "" {
		command = "tesseract"
	}
	text, err := runOcrCommand(command, []string{
		inputPath, "stdout", "--psm", fmt.Sprintf("%d", psm), "-l", language,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Text: CleanOcrText(text), Language: language, PSM: psm}, nil
}

func runOcrCommand(command string, args []string) (string, error) {
	cmd := exec.Command(command, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedBuffer{limit: maxOutputBytes, inner: &stdout}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("OCR command failed to start: %s", err.Error())
	}
	if err := cmd.Wait(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		code := -1
		var exitError *exec.ExitError
		if errorsAs(err, &exitError) {
			code = exitError.ExitCode()
		}
		if detail != "" {
			return "", fmt.Errorf("OCR command exited with %d: %s", code, detail)
		}
		return "", fmt.Errorf("OCR command exited with %d", code)
	}
	return stdout.String(), nil
}

// limitedBuffer caps how much output is retained (1 MB stdout cap).
type limitedBuffer struct {
	limit   int
	inner   interface{ Write([]byte) (int, error) }
	written int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.written < b.limit {
		room := b.limit - b.written
		if len(p) <= room {
			b.written += len(p)
			return b.inner.Write(p)
		}
		b.written += room
		_, _ = b.inner.Write(p[:room])
	}
	return len(p), nil
}

func errorsAs(err error, target **exec.ExitError) bool {
	exitError, ok := err.(*exec.ExitError)
	if ok {
		*target = exitError
	}
	return ok
}

// CleanOcrText normalizes each line's whitespace and drops empty lines.
func CleanOcrText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		cleaned := strings.TrimSpace(collapseSpaces(line))
		if cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return strings.Join(out, "\n")
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\v', '\f', '\r', 0x00a0:
			pending = true
		default:
			if pending {
				b.WriteByte(' ')
				pending = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
