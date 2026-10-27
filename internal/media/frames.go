package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// SampleWidth/SampleHeight are the analysis sample dimensions every hdmi_*
// comparison operates on.
const (
	SampleWidth  = 160
	SampleHeight = 90
	FrameBytes   = SampleWidth * SampleHeight
)

// VideoFrame is one decoded gray8 sample frame.
type VideoFrame struct {
	Sequence     int
	CapturedAtMs int64
	Width        int
	Height       int
	Format       string // "gray8"
	Data         []byte
}

// FrameSource yields gray8 sample frames from the video track.
type FrameSource interface {
	Width() int
	Height() int
	Format() string
	Next(timeoutMs int64) (*VideoFrame, error)
	Close()
}

// WebRtcFrameSource decodes the track into fixed-size gray8 frames.
type WebRtcFrameSource struct {
	decoder  *H264FrameDecoder
	mu       sync.Mutex
	sequence int
	closed   bool
}

// NewWebRtcFrameSource wraps a video packet stream in a gray8 decoder.
func NewWebRtcFrameSource(packets <-chan *rtp.Packet, packetsClosed <-chan struct{}) (*WebRtcFrameSource, error) {
	decoder, err := NewH264FrameDecoder(packets, packetsClosed, []string{
		"-vf", "scale=160:90:flags=bilinear,format=gray",
		"-f", "rawvideo", "-pix_fmt", "gray", "pipe:1",
	}, FixedSizeFrameParser(FrameBytes))
	if err != nil {
		return nil, err
	}
	return &WebRtcFrameSource{decoder: decoder}, nil
}

func (s *WebRtcFrameSource) Width() int     { return SampleWidth }
func (s *WebRtcFrameSource) Height() int    { return SampleHeight }
func (s *WebRtcFrameSource) Format() string { return "gray8" }

// Next waits for the next decoded gray frame.
func (s *WebRtcFrameSource) Next(timeoutMs int64) (*VideoFrame, error) {
	data, err := s.decoder.Next(timeoutMs)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	s.mu.Lock()
	s.sequence++
	seq := s.sequence
	s.mu.Unlock()
	return &VideoFrame{
		Sequence:     seq,
		CapturedAtMs: time.Now().UnixMilli(),
		Width:        SampleWidth,
		Height:       SampleHeight,
		Format:       "gray8",
		Data:         data,
	}, nil
}

// Close kills the decoder.
func (s *WebRtcFrameSource) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	if s.decoder != nil {
		s.decoder.Close()
	}
}

// PNGSource yields complete PNG images from the video track.
type PNGSource interface {
	Next(timeoutMs int64) ([]byte, error)
	Close()
}

// WebRtcPNGSource decodes the track into a stream of PNG stills.
type WebRtcPNGSource struct {
	decoder *H264FrameDecoder
}

// NewWebRtcPNGSource wraps a video packet stream in a PNG decoder.
func NewWebRtcPNGSource(packets <-chan *rtp.Packet, packetsClosed <-chan struct{}) (*WebRtcPNGSource, error) {
	decoder, err := NewH264FrameDecoder(packets, packetsClosed, []string{
		"-f", "image2pipe", "-vcodec", "png", "pipe:1",
	}, ParsePNGFrames)
	if err != nil {
		return nil, err
	}
	return &WebRtcPNGSource{decoder: decoder}, nil
}

// Next waits for the next complete PNG image.
func (s *WebRtcPNGSource) Next(timeoutMs int64) ([]byte, error) {
	return s.decoder.Next(timeoutMs)
}

// Close kills the decoder.
func (s *WebRtcPNGSource) Close() {
	if s.decoder != nil {
		s.decoder.Close()
	}
}

var pngSignature = []byte{137, 80, 78, 71, 13, 10, 26, 10}

// ParsePNGFrames splits an image2pipe stream into complete PNG images by
// signature + IEND chunk scanning.
func ParsePNGFrames(output []byte) ([][]byte, []byte) {
	var frames [][]byte
	cursor := 0

	for cursor < len(output) {
		signatureStart := bytes.Index(output[cursor:], pngSignature)
		if signatureStart < 0 {
			keep := len(output) - len(pngSignature) + 1
			if keep < 0 {
				keep = 0
			}
			if keep < cursor {
				keep = cursor
			}
			return frames, output[keep:]
		}
		signatureStart += cursor

		chunkStart := signatureStart + len(pngSignature)
		complete := false
		for chunkStart+12 <= len(output) {
			chunkLength := binary.BigEndian.Uint32(output[chunkStart:])
			chunkEnd := chunkStart + 12 + int(chunkLength)
			if chunkEnd > len(output) || chunkEnd < 0 {
				break
			}
			chunkType := string(output[chunkStart+4 : chunkStart+8])
			chunkStart = chunkEnd
			if chunkType == "IEND" {
				frame := make([]byte, chunkStart-signatureStart)
				copy(frame, output[signatureStart:chunkStart])
				frames = append(frames, frame)
				cursor = chunkStart
				complete = true
				break
			}
		}

		if !complete {
			return frames, output[signatureStart:]
		}
	}

	return frames, nil
}

// PNGFrameHub keeps one PNG decoder alive and serves the newest complete
// frame to callers.
type PNGFrameHub struct {
	source PNGSource

	mu      sync.Mutex
	waiters []chan hubResult
	latest  []byte
	closed  bool
	failure error

	closeOnce sync.Once
	closeDone chan struct{}
	loopDone  chan struct{}
}

type hubResult struct {
	frame []byte
	err   error
}

// NewPNGFrameHub starts the hub read loop.
func NewPNGFrameHub(source PNGSource) *PNGFrameHub {
	h := &PNGFrameHub{
		source:    source,
		closeDone: make(chan struct{}),
		loopDone:  make(chan struct{}),
	}
	go h.readLoop()
	return h
}

// Next returns the latest frame immediately when one exists, else waits.
func (h *PNGFrameHub) Next(timeoutMs int64) ([]byte, error) {
	h.mu.Lock()
	if h.failure != nil {
		err := h.failure
		h.mu.Unlock()
		return nil, err
	}
	if h.latest != nil {
		frame := make([]byte, len(h.latest))
		copy(frame, h.latest)
		h.mu.Unlock()
		return frame, nil
	}
	if h.closed {
		h.mu.Unlock()
		return nil, nil
	}
	if timeoutMs <= 0 {
		timeoutMs = 1
	}
	ch := make(chan hubResult, 1)
	h.waiters = append(h.waiters, ch)
	h.mu.Unlock()

	select {
	case res := <-ch:
		return res.frame, res.err
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		h.mu.Lock()
		for i, w := range h.waiters {
			if w == ch {
				h.waiters = append(h.waiters[:i], h.waiters[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		return nil, nil
	}
}

// Close stops the loop and the underlying source; safe to call repeatedly.
func (h *PNGFrameHub) Close() {
	h.closeOnce.Do(func() {
		close(h.closeDone)
		h.mu.Lock()
		h.closed = true
		h.latest = nil
		waiters := h.waiters
		h.waiters = nil
		h.mu.Unlock()
		for _, w := range waiters {
			w <- hubResult{}
		}
		go func() {
			h.source.Close()
			<-h.loopDone
		}()
	})
	// Drain loopDone quickly so callers do not block on hub internals.
	<-h.loopDone
}

func (h *PNGFrameHub) readLoop() {
	defer close(h.loopDone)
	for {
		select {
		case <-h.closeDone:
			return
		default:
		}
		frame, err := h.source.Next(1_000)
		if err != nil {
			h.fail(err)
			return
		}
		if frame == nil {
			continue
		}
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return
		}
		h.latest = make([]byte, len(frame))
		copy(h.latest, frame)
		waiters := h.waiters
		h.waiters = nil
		h.mu.Unlock()
		for _, w := range waiters {
			w <- hubResult{frame: h.latest}
		}
	}
}

func (h *PNGFrameHub) fail(err error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.failure = err
	h.closed = true
	h.latest = nil
	waiters := h.waiters
	h.waiters = nil
	h.mu.Unlock()
	for _, w := range waiters {
		w <- hubResult{err: err}
	}
	go func() {
		h.source.Close()
		<-h.loopDone
	}()
}

// ReadPNGDimensions reads width/height from the IHDR header, validating the
// signature.
func ReadPNGDimensions(image []byte) (int, int, error) {
	if len(image) < 24 || !bytes.HasPrefix(image, pngSignature) {
		return 0, 0, fmt.Errorf("HDMI frame is not a PNG image")
	}
	width := binary.BigEndian.Uint32(image[16:20])
	height := binary.BigEndian.Uint32(image[20:24])
	return int(width), int(height), nil
}
