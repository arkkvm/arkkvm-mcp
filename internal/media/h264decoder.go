// Package media turns the live WebRTC HDMI video track into decoded frames
// (PNG stills or 160x90 gray8 samples) via a long-lived ffmpeg process, and
// provides the pixel-difference frame analysis used by the hdmi_* tools.
package media

import (
	"encoding/binary"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/rtp"
	rtpcodecs "github.com/pion/rtp/codecs"

	"arkkvm-mcp/internal/logger"
)

// DecodedFrameParser splits the ffmpeg stdout stream into complete frames and
// returns the unconsumed tail.
type DecodedFrameParser func(output []byte) (frames [][]byte, remaining []byte)

type frameResult struct {
	frame []byte
	err   error
}

type frameWaiter struct {
	ch chan frameResult
}

// H264FrameDecoder converts a live video track into a stream of decoded
// frames. The decoder stays open across RTP frames so ffmpeg can retain H264
// state.
type H264FrameDecoder struct {
	parse DecodedFrameParser

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	mu                 sync.Mutex
	output             []byte
	frames             [][]byte
	waiters            []*frameWaiter
	packetsByTimestamp map[uint32][]*rtp.Packet
	started            bool
	closed             bool
	failure            error

	h264p rtpcodecs.H264Packet
	// packets is the fan-out subscription channel owned by the device
	// client's video pump; packetsClosed signals the pump stopped.
	packets       <-chan *rtp.Packet
	packetsClosed <-chan struct{}
	wg            sync.WaitGroup
}

// FFmpeg base args shared by every decoder variant. nobuffer/low_delay only
// shrink the decode buffers; they cannot skip find_stream_info, which by
// default buffers probesize (5MB) of input before probing ends and emits no
// frame until then. Agent sessions only receive keyframes (~1 frame/s), so
// filling 5MB takes over 30s — the entire cause of the slow first frame. The
// format is already pinned via -f h264, so probing is unnecessary.
func ffmpegBaseArgs(outputArgs []string) []string {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0",
		"-fflags", "nobuffer", "-flags", "low_delay",
		"-f", "h264", "-i", "pipe:0",
	}
	return append(args, outputArgs...)
}

// NewH264FrameDecoder spawns ffmpeg and starts consuming the packet stream.
func NewH264FrameDecoder(packets <-chan *rtp.Packet, packetsClosed <-chan struct{}, outputArgs []string, parse DecodedFrameParser) (*H264FrameDecoder, error) {
	cmd := exec.Command("ffmpeg", ffmpegBaseArgs(outputArgs)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard // matched: stderr drained and ignored
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	d := &H264FrameDecoder{
		parse:              parse,
		cmd:                cmd,
		stdin:              stdin,
		stdout:             stdout,
		packetsByTimestamp: make(map[uint32][]*rtp.Packet),
		packets:            packets,
		packetsClosed:      packetsClosed,
	}
	d.wg.Add(2)
	go d.stdoutPump()
	go d.packetReader()
	return d, nil
}

func (d *H264FrameDecoder) stdoutPump() {
	defer d.wg.Done()
	buf := make([]byte, 64*1024)
	for {
		n, err := d.stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			d.onDecodedData(chunk)
		}
		if err != nil {
			// Process exited (normal on close) or pipe broke.
			d.mu.Lock()
			closed := d.closed
			d.mu.Unlock()
			if !closed {
				// ffmpeg died unexpectedly with output still streaming.
				if err == io.EOF {
					d.fail(fmt.Errorf("H264 decoder exited"))
				}
			}
			return
		}
	}
}

func (d *H264FrameDecoder) packetReader() {
	defer d.wg.Done()
	for {
		select {
		case packet, ok := <-d.packets:
			if !ok {
				// Pump stopped (track closed / session dead); stop feeding.
				return
			}
			d.onRtpPacket(packet)
		case <-d.packetsClosed:
			return
		}
	}
}

// Next returns the next decoded frame, or nil when the timeout elapses first.
func (d *H264FrameDecoder) Next(timeoutMs int64) ([]byte, error) {
	d.mu.Lock()
	if len(d.frames) > 0 {
		frame := d.frames[0]
		d.frames = d.frames[1:]
		d.mu.Unlock()
		return frame, nil
	}
	if d.failure != nil {
		err := d.failure
		d.mu.Unlock()
		return nil, err
	}
	if d.closed {
		d.mu.Unlock()
		return nil, nil
	}
	if timeoutMs <= 0 {
		timeoutMs = 1
	}
	waiter := &frameWaiter{ch: make(chan frameResult, 1)}
	d.waiters = append(d.waiters, waiter)
	d.mu.Unlock()

	select {
	case res := <-waiter.ch:
		return res.frame, res.err
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		d.mu.Lock()
		for i, w := range d.waiters {
			if w == waiter {
				d.waiters = append(d.waiters[:i], d.waiters[i+1:]...)
				break
			}
		}
		d.mu.Unlock()
		return nil, nil
	}
}

// Close kills ffmpeg and releases every waiter.
func (d *H264FrameDecoder) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.packetsByTimestamp = make(map[uint32][]*rtp.Packet)
	waiters := d.waiters
	d.waiters = nil
	d.frames = nil
	cmd := d.cmd
	d.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	for _, w := range waiters {
		w.ch <- frameResult{frame: nil}
	}
}

func (d *H264FrameDecoder) onRtpPacket(packet *rtp.Packet) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	timestamp := packet.Timestamp
	packets := d.packetsByTimestamp[timestamp]
	packets = append(packets, packet)
	d.packetsByTimestamp[timestamp] = packets
	if !packet.Marker {
		d.mu.Unlock()
		return
	}
	delete(d.packetsByTimestamp, timestamp)
	group := packets
	d.mu.Unlock()

	var frameData []byte
	for _, p := range group {
		data, err := d.h264p.Unmarshal(p.Payload)
		if err != nil {
			d.fail(err)
			return
		}
		frameData = append(frameData, data...)
	}
	if len(frameData) == 0 {
		return
	}
	if isH264Keyframe(frameData) {
		d.mu.Lock()
		d.started = true
		d.mu.Unlock()
	}
	d.mu.Lock()
	started := d.started
	closed := d.closed
	d.mu.Unlock()
	if !started || closed {
		return
	}
	// Feed Annex-B to ffmpeg; EPIPE after kill is harmless.
	_, _ = d.stdin.Write(frameData)
}

func (d *H264FrameDecoder) onDecodedData(chunk []byte) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.output = append(d.output, chunk...)
	frames, remaining, err := splitFrames(d.parse, d.output)
	if err != nil {
		d.mu.Unlock()
		d.fail(err)
		return
	}
	d.output = remaining
	waiters := d.waiters
	d.waiters = nil
	for _, frame := range frames {
		if len(waiters) > 0 {
			w := waiters[0]
			waiters = waiters[1:]
			w.ch <- frameResult{frame: frame}
		} else {
			// Keep at most two decoded frames buffered.
			if len(d.frames) >= 2 {
				d.frames = d.frames[1:]
			}
			d.frames = append(d.frames, frame)
		}
	}
	d.waiters = waiters
	d.mu.Unlock()
}

func splitFrames(parse DecodedFrameParser, output []byte) ([][]byte, []byte, error) {
	frames, remaining := parse(output)
	return frames, remaining, nil
}

func (d *H264FrameDecoder) fail(err error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.failure = err
	d.closed = true
	waiters := d.waiters
	d.waiters = nil
	d.frames = nil
	d.packetsByTimestamp = make(map[uint32][]*rtp.Packet)
	cmd := d.cmd
	d.mu.Unlock()

	logger.Warn("H264 decoder failed", map[string]any{"error": err.Error()})
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	for _, w := range waiters {
		w.ch <- frameResult{err: err}
	}
}

// isH264Keyframe reports whether the Annex-B payload contains an IDR NAL
// (type 5). SPS/PPS (7/8) alone do not count.
func isH264Keyframe(annexb []byte) bool {
	const startCode = 0x00000001
	i := 0
	for i+4 <= len(annexb) {
		if binary.BigEndian.Uint32(annexb[i:]) != startCode {
			i++
			continue
		}
		i += 4
		if i < len(annexb) {
			nalType := annexb[i] & 0x1f
			if nalType == 5 {
				return true
			}
		}
	}
	return false
}
