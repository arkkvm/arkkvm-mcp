package media

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func pngFrame(width, height uint32, payload []byte) []byte {
	var out bytes.Buffer
	out.Write(pngSignature)
	chunk := make([]byte, 0, 12+len(payload))
	var length bytes.Buffer
	_ = binary.Write(&length, binary.BigEndian, uint32(len(payload)))
	chunk = append(chunk, length.Bytes()...)
	chunk = append(chunk, []byte("IHDR")...)
	chunk = append(chunk, payload...)
	chunk = append(chunk, 0, 0, 0, 0) // CRC placeholder
	out.Write(chunk)
	var iend bytes.Buffer
	_ = binary.Write(&iend, binary.BigEndian, uint32(0))
	out.Write(iend.Bytes())
	out.Write([]byte("IEND"))
	out.Write([]byte{0, 0, 0, 0})
	// Patch IHDR dims (bytes 16..24 hold width/height).
	buf := out.Bytes()
	binary.BigEndian.PutUint32(buf[16:20], width)
	binary.BigEndian.PutUint32(buf[20:24], height)
	return buf
}

func TestParsePNGFrames(t *testing.T) {
	frame1 := pngFrame(64, 32, []byte("dims"))
	frame2 := pngFrame(320, 240, []byte("dims"))
	stream := append(append([]byte{}, frame1...), frame2...)
	frames, remaining := ParsePNGFrames(stream)
	if len(frames) != 2 {
		t.Fatalf("ParsePNGFrames got %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0], frame1) || !bytes.Equal(frames[1], frame2) {
		t.Error("frames must round-trip byte-for-byte")
	}
	if len(remaining) != 0 {
		t.Errorf("remaining must be empty, got %d bytes", len(remaining))
	}
	// A partial frame stays in remaining.
	frames, remaining = ParsePNGFrames(append(append([]byte{}, frame1...), frame2[:20]...))
	if len(frames) != 1 {
		t.Errorf("partial stream frames = %d, want 1", len(frames))
	}
	if len(remaining) != 20 {
		t.Errorf("partial remaining = %d, want 20", len(remaining))
	}
	// Garbage before the signature is skipped; trailing garbage is dropped
	// except a potential signature prefix.
	frames, _ = ParsePNGFrames(append([]byte{1, 2, 3}, frame1...))
	if len(frames) != 1 {
		t.Errorf("garbage-prefixed frames = %d, want 1", len(frames))
	}
}

func TestFixedSizeFrameParser(t *testing.T) {
	parse := FixedSizeFrameParser(4)
	frames, remaining := parse([]byte{1, 2, 3, 4, 5, 6})
	if len(frames) != 1 || !bytes.Equal(frames[0], []byte{1, 2, 3, 4}) {
		t.Errorf("frames = %v", frames)
	}
	if !bytes.Equal(remaining, []byte{5, 6}) {
		t.Errorf("remaining = %v", remaining)
	}
}

func TestReadPNGDimensions(t *testing.T) {
	w, h, err := ReadPNGDimensions(pngFrame(640, 360, []byte("x")))
	if err != nil || w != 640 || h != 360 {
		t.Errorf("ReadPNGDimensions = %d, %d, %v", w, h, err)
	}
	if _, _, err := ReadPNGDimensions([]byte("not a png at all")); err == nil {
		t.Error("non-PNG must fail")
	}
}

func TestRoundDiv(t *testing.T) {
	cases := []struct {
		a, b, want int
	}{
		{10, 2, 5}, {11, 2, 6}, {9, 2, 5}, {0, 7, 0}, {7, 160, 0}, {80, 160, 1},
	}
	for _, c := range cases {
		if got := roundDiv(c.a, c.b); got != c.want {
			t.Errorf("roundDiv(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareGrayFramesErrors(t *testing.T) {
	frame := func() *VideoFrame {
		return &VideoFrame{Width: 4, Height: 4, Data: make([]byte, 16)}
	}
	if _, err := CompareGrayFrames(frame(), frame(), 0, 16, nil); err == nil {
		t.Error("threshold 0 must fail")
	}
	if _, err := CompareGrayFrames(frame(), frame(), 1.5, 16, nil); err == nil {
		t.Error("threshold > 1 must fail")
	}
	if _, err := CompareGrayFrames(frame(), frame(), 0.1, 300, nil); err == nil {
		t.Error("pixelThreshold > 255 must fail")
	}
	other := &VideoFrame{Width: 5, Height: 4, Data: make([]byte, 20)}
	if _, err := CompareGrayFrames(frame(), other, 0.1, 16, nil); err == nil {
		t.Error("dimension mismatch must fail")
	}
}

func TestCompareGrayFramesChangeDetection(t *testing.T) {
	before := &VideoFrame{Width: 4, Height: 4, Data: make([]byte, 16)}
	after := &VideoFrame{Width: 4, Height: 4, Data: make([]byte, 16)}
	// Flip a quarter of the pixels far above the threshold.
	for i := 0; i < 4; i++ {
		after.Data[i] = 200
	}
	comparison, err := CompareGrayFrames(before, after, 0.2, 16, nil)
	if err != nil {
		t.Fatalf("CompareGrayFrames: %v", err)
	}
	if !comparison.Changed {
		t.Error("25% changed pixels must exceed a 0.2 threshold")
	}
	if comparison.ChangedPixels != 4 || comparison.ComparedPixels != 16 {
		t.Errorf("counts = %d/%d", comparison.ChangedPixels, comparison.ComparedPixels)
	}
	if comparison.Score < 0.24 || comparison.Score > 0.26 {
		t.Errorf("score = %v", comparison.Score)
	}
	// Identical frames never change.
	comparison, err = CompareGrayFrames(before, before, 0.03, 16, nil)
	if err != nil || comparison.Changed {
		t.Errorf("identical frames: %v, %v", comparison.Changed, err)
	}
}
