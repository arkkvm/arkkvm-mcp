package media

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"time"
)

// FixedSizeFrameParser splits rawvideo output into fixed-size gray frames.
func FixedSizeFrameParser(frameBytes int) DecodedFrameParser {
	return func(output []byte) ([][]byte, []byte) {
		var frames [][]byte
		remaining := output
		for len(remaining) >= frameBytes {
			frame := make([]byte, frameBytes)
			copy(frame, remaining[:frameBytes])
			frames = append(frames, frame)
			remaining = remaining[frameBytes:]
		}
		return frames, remaining
	}
}

// Region is a rectangle in frame coordinates.
type Region struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// FrameComparison is the shared result shape of every hdmi comparison tool.
type FrameComparison struct {
	Changed        bool       `json:"changed"`
	Score          float64    `json:"score"`
	MeanDifference float64    `json:"meanDifference"`
	ChangedPixels  int        `json:"changedPixels"`
	ComparedPixels int        `json:"comparedPixels"`
	Dimensions     Dimensions `json:"dimensions"`
	ChangedRegions []Region   `json:"changedRegions"`
}

// Dimensions carries the before/after frame sizes.
type Dimensions struct {
	Before Size `json:"before"`
	After  Size `json:"after"`
}

// Size is one frame size.
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// StreamAnalysis is the hdmi_video_monitor result.
type StreamAnalysis struct {
	DurationMs     int64    `json:"durationMs"`
	Frames         int      `json:"frames"`
	Comparisons    int      `json:"comparisons"`
	ChangedFrames  int      `json:"changedFrames"`
	MaxScore       float64  `json:"maxScore"`
	LastScore      float64  `json:"lastScore"`
	ChangedRegions []Region `json:"changedRegions"`
}

// CompareOptions tune the pixel-difference analyzer.
type CompareOptions struct {
	Threshold      float64
	PixelThreshold int
	Region         *Region
}

// UnchangedHdmiFrame builds the neutral comparison for a single frame.
func UnchangedHdmiFrame(image []byte) (FrameComparison, error) {
	width, height, err := ReadPNGDimensions(image)
	if err != nil {
		return FrameComparison{}, err
	}
	return FrameComparison{
		Changed:        false,
		Score:          0,
		MeanDifference: 0,
		ChangedPixels:  0,
		ComparedPixels: SampleWidth * SampleHeight,
		Dimensions: Dimensions{
			Before: Size{Width: width, Height: height},
			After:  Size{Width: width, Height: height},
		},
		ChangedRegions: []Region{},
	}, nil
}

// DecodeToGray converts one PNG into a 160x90 gray8 buffer via ffmpeg.
func DecodeToGray(image []byte) ([]byte, error) {
	cmd := exec.Command("ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-f", "image2pipe",
		"-i", "pipe:0",
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=bilinear,format=gray", SampleWidth, SampleHeight),
		"-f", "rawvideo",
		"-pix_fmt", "gray",
		"pipe:1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Write then read; the frame is small enough to avoid deadlock risk via
	// the pipe buffer, but write in a goroutine regardless.
	writeDone := make(chan error, 1)
	go func() {
		_, werr := stdin.Write(image)
		_ = stdin.Close()
		writeDone <- werr
	}()
	output, err := io.ReadAll(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	if werr := <-writeDone; werr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, werr
	}
	exitErr := cmd.Wait()
	if exitErr != nil {
		code := -1
		var exitError *exec.ExitError
		if errors.As(exitErr, &exitError) {
			code = exitError.ExitCode()
		}
		return nil, fmt.Errorf("FFmpeg could not decode an HDMI PNG frame (exit %d)", code)
	}
	if len(output) != SampleWidth*SampleHeight {
		return nil, fmt.Errorf("Unexpected grayscale HDMI frame size: %d", len(output))
	}
	return output, nil
}

func clampRegion(region *Region, width, height int) Region {
	if region == nil {
		return Region{X: 0, Y: 0, Width: width, Height: height}
	}
	x := clampInt(region.X, 0, width)
	y := clampInt(region.Y, 0, height)
	right := clampInt(region.X+region.Width, x, width)
	bottom := clampInt(region.Y+region.Height, y, height)
	return Region{X: x, Y: y, Width: right - x, Height: bottom - y}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func toSampleRegion(region Region, width, height int) Region {
	x := region.X * SampleWidth / width
	y := region.Y * SampleHeight / height
	// Go integer division truncates toward zero, matching Math.floor for the
	// non-negative coordinates used here.
	right := (region.X + region.Width) * SampleWidth / width
	if right < x+1 {
		right = x + 1
	}
	bottom := (region.Y + region.Height) * SampleHeight / height
	if bottom < y+1 {
		bottom = y + 1
	}
	return clampRegion(&Region{X: x, Y: y, Width: right - x, Height: bottom - y}, SampleWidth, SampleHeight)
}

func blockRegion(column, row int, region Region) Region {
	x := region.X + column*region.Width/GRIDColumns
	y := region.Y + row*region.Height/GRIDRows
	right := region.X + (column+1)*region.Width/GRIDColumns
	if right == x {
		right = x + 1
	}
	bottom := region.Y + (row+1)*region.Height/GRIDRows
	if bottom == y {
		bottom = y + 1
	}
	w, h := right-x, bottom-y
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return Region{X: x, Y: y, Width: w, Height: h}
}

// Grid geometry for changed-region reporting.
const (
	GRIDColumns = 8
	GRIDRows    = 6
)

type regionDiff struct {
	meanDifference float64
	changedPixels  int
	comparedPixels int
}

func diffForRegion(before, after []byte, region Region, width int, pixelThreshold int) regionDiff {
	var sum int
	changedPixels := 0
	comparedPixels := 0
	for y := region.Y; y < region.Y+region.Height; y++ {
		row := y * width
		for x := region.X; x < region.X+region.Width; x++ {
			difference := int(before[row+x]) - int(after[row+x])
			if difference < 0 {
				difference = -difference
			}
			sum += difference
			if difference >= pixelThreshold {
				changedPixels++
			}
			comparedPixels++
		}
	}
	mean := 0.0
	if comparedPixels != 0 {
		mean = float64(sum) / float64(comparedPixels) / 255
	}
	return regionDiff{meanDifference: mean, changedPixels: changedPixels, comparedPixels: comparedPixels}
}

// CompareGrayFrames diffs two sample-space gray frames.
func CompareGrayFrames(before, after *VideoFrame, threshold float64, pixelThreshold int, requestedRegion *Region) (FrameComparison, error) {
	if threshold <= 0 || threshold > 1 {
		return FrameComparison{}, fmt.Errorf("HDMI change threshold must be greater than 0 and at most 1")
	}
	if pixelThreshold < 0 || pixelThreshold > 255 {
		return FrameComparison{}, fmt.Errorf("HDMI pixel threshold must be between 0 and 255")
	}
	if before.Width != after.Width || before.Height != after.Height {
		return FrameComparison{}, fmt.Errorf("HDMI frame dimensions differ: %dx%d vs %dx%d", before.Width, before.Height, after.Width, after.Height)
	}
	expectedBytes := before.Width * before.Height
	if len(before.Data) != expectedBytes || len(after.Data) != expectedBytes {
		return FrameComparison{}, fmt.Errorf("HDMI gray frame has an invalid size")
	}
	region := clampRegion(requestedRegion, after.Width, after.Height)
	diff := diffForRegion(before.Data, after.Data, region, after.Width, pixelThreshold)
	changedRegions := []Region{}
	for row := 0; row < GRIDRows; row++ {
		for column := 0; column < GRIDColumns; column++ {
			block := blockRegion(column, row, region)
			blockDiff := diffForRegion(before.Data, after.Data, block, after.Width, pixelThreshold)
			if blockDiff.comparedPixels > 0 && float64(blockDiff.changedPixels)/float64(blockDiff.comparedPixels) >= threshold {
				changedRegions = append(changedRegions, block)
			}
		}
	}
	changed := diff.comparedPixels > 0 && float64(diff.changedPixels)/float64(diff.comparedPixels) >= threshold
	score := 0.0
	if diff.comparedPixels != 0 {
		score = float64(diff.changedPixels) / float64(diff.comparedPixels)
	}
	return FrameComparison{
		Changed:        changed,
		Score:          score,
		MeanDifference: diff.meanDifference,
		ChangedPixels:  diff.changedPixels,
		ComparedPixels: diff.comparedPixels,
		Dimensions: Dimensions{
			Before: Size{Width: before.Width, Height: before.Height},
			After:  Size{Width: after.Width, Height: after.Height},
		},
		ChangedRegions: changedRegions,
	}, nil
}

// ComparePNGFrames decodes two PNG stills to sample space and diffs them.
// changedRegions map back to source resolution.
func ComparePNGFrames(beforeImage, afterImage []byte, threshold float64, pixelThreshold int, requestedRegion *Region) (FrameComparison, error) {
	if threshold <= 0 || threshold > 1 {
		return FrameComparison{}, fmt.Errorf("HDMI change threshold must be greater than 0 and at most 1")
	}
	if pixelThreshold < 0 || pixelThreshold > 255 {
		return FrameComparison{}, fmt.Errorf("HDMI pixel threshold must be between 0 and 255")
	}
	beforeW, beforeH, err := ReadPNGDimensions(beforeImage)
	if err != nil {
		return FrameComparison{}, err
	}
	afterW, afterH, err := ReadPNGDimensions(afterImage)
	if err != nil {
		return FrameComparison{}, err
	}
	if beforeW != afterW || beforeH != afterH {
		return FrameComparison{}, fmt.Errorf("HDMI frame dimensions differ: %dx%d vs %dx%d", beforeW, beforeH, afterW, afterH)
	}
	before, err := DecodeToGray(beforeImage)
	if err != nil {
		return FrameComparison{}, err
	}
	after, err := DecodeToGray(afterImage)
	if err != nil {
		return FrameComparison{}, err
	}
	requested := clampRegion(requestedRegion, afterW, afterH)
	region := toSampleRegion(requested, afterW, afterH)
	diff := diffForRegion(before, after, region, SampleWidth, pixelThreshold)
	changedRegions := []Region{}
	for row := 0; row < GRIDRows; row++ {
		for column := 0; column < GRIDColumns; column++ {
			block := blockRegion(column, row, region)
			blockDiff := diffForRegion(before, after, block, SampleWidth, pixelThreshold)
			if blockDiff.comparedPixels > 0 && float64(blockDiff.changedPixels)/float64(blockDiff.comparedPixels) >= threshold {
				changedRegions = append(changedRegions, Region{
					X:      roundDiv(block.X*afterW, SampleWidth),
					Y:      roundDiv(block.Y*afterH, SampleHeight),
					Width:  roundDiv(block.Width*afterW, SampleWidth),
					Height: roundDiv(block.Height*afterH, SampleHeight),
				})
			}
		}
	}
	changed := diff.comparedPixels > 0 && float64(diff.changedPixels)/float64(diff.comparedPixels) >= threshold
	score := 0.0
	if diff.comparedPixels != 0 {
		score = float64(diff.changedPixels) / float64(diff.comparedPixels)
	}
	return FrameComparison{
		Changed:        changed,
		Score:          score,
		MeanDifference: diff.meanDifference,
		ChangedPixels:  diff.changedPixels,
		ComparedPixels: diff.comparedPixels,
		Dimensions: Dimensions{
			Before: Size{Width: beforeW, Height: beforeH},
			After:  Size{Width: afterW, Height: afterH},
		},
		ChangedRegions: changedRegions,
	}, nil
}

// roundDiv performs division with Math.round semantics for non-negative values.
func roundDiv(a, b int) int {
	if b == 0 {
		return 0
	}
	return (a + b/2) / b
}

// AnalyzeFrameStream samples a frame source for durationMs and reports
// per-comparison change statistics (hdmi_video_monitor core).
func AnalyzeFrameStream(source FrameSource, durationMs int64, opts CompareOptions, maxEvents int) (StreamAnalysis, error) {
	if maxEvents <= 0 {
		maxEvents = 20
	}
	if durationMs <= 0 {
		durationMs = 5_000
	}
	startedAt := time.Now()
	deadline := startedAt.Add(time.Duration(durationMs) * time.Millisecond)
	remaining := func() int64 {
		r := time.Until(deadline).Milliseconds()
		if r < 1 {
			r = 1
		}
		return r
	}

	frames := 0
	comparisons := 0
	changedFrames := 0
	maxScore := 0.0
	lastScore := 0.0
	changedRegions := []Region{}

	previous, err := source.Next(remaining())
	if err != nil {
		return StreamAnalysis{}, err
	}
	if previous != nil {
		frames++
	}

	for previous != nil && time.Now().Before(deadline) {
		current, err := source.Next(remaining())
		if err != nil {
			return StreamAnalysis{}, err
		}
		if current == nil {
			break
		}
		frames++
		comparison, err := CompareGrayFrames(previous, current, opts.Threshold, opts.PixelThreshold, opts.Region)
		if err != nil {
			return StreamAnalysis{}, err
		}
		comparisons++
		lastScore = comparison.Score
		maxScore = math.Max(maxScore, comparison.Score)
		if comparison.Changed {
			changedFrames++
			for _, region := range comparison.ChangedRegions {
				if len(changedRegions) >= maxEvents {
					break
				}
				changedRegions = append(changedRegions, region)
			}
		}
		previous = current
	}

	return StreamAnalysis{
		DurationMs:     time.Since(startedAt).Milliseconds(),
		Frames:         frames,
		Comparisons:    comparisons,
		ChangedFrames:  changedFrames,
		MaxScore:       maxScore,
		LastScore:      lastScore,
		ChangedRegions: changedRegions,
	}, nil
}
