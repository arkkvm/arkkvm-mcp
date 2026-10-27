package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"arkkvm-mcp/internal/device"
	"arkkvm-mcp/internal/media"
	"arkkvm-mcp/internal/ocr"
	"arkkvm-mcp/internal/textmatch"
)

const screenshotSchemaTail = "" +
	`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":60000,"description":"Maximum time to wait for a decodable HDMI frame in milliseconds. Default is 60000; the first capture in a fresh MCP process takes a few seconds while the device emits its next keyframe to the agent session."}`

const hdmiRegionSchema = `{"type":"object","properties":{"x":{"type":"integer","minimum":0},"y":{"type":"integer","minimum":0},"width":{"type":"integer","exclusiveMinimum":0},"height":{"type":"integer","exclusiveMinimum":0}},"required":["x","y","width","height"],"additionalProperties":false}`

func defaultOCRLanguage() string {
	if lang := os.Getenv("ARKKVM_OCR_LANG"); lang != "" {
		return lang
	}
	return "eng+chi_sim"
}

func parseRegion(args map[string]any, key string) (*media.Region, error) {
	value, ok := argMap(args, key)
	if !ok {
		return nil, nil
	}
	x, _ := argInt(value, "x")
	y, _ := argInt(value, "y")
	width, hasW := argInt(value, "width")
	height, hasH := argInt(value, "height")
	if x < 0 || y < 0 || !hasW || width <= 0 || !hasH || height <= 0 {
		return nil, fmt.Errorf("region must be {x>=0, y>=0, width>0, height>0}")
	}
	return &media.Region{X: int(x), Y: int(y), Width: int(width), Height: int(height)}, nil
}

func ocrOptions(language string, psm *int) ocr.Options {
	opts := ocr.Options{Language: language}
	if psm != nil {
		opts.PSM = psm
	}
	return opts
}

func (s *Server) registerHdmiTools() {
	register(s.mcp, "hdmi_screenshot",
		"Capture one current screenshot from the HDMI-connected controlled machine. The ArkKVM device receives HDMI and sends H.264 video over the existing WebRTC media track; this tool receives and decodes one frame on the controller machine, then returns a PNG image for visual inspection or AI analysis. It does not capture the controller desktop or the MCP Inspector page. Use this after device connectivity is available. A successful return means a video frame was decoded, but the image content itself must be inspected to determine what is displayed. If it times out, check HDMI signal/video state and retry. This tool consumes only the HDMI video track and does not send HID input.",
		`{"type":"object","properties":{`+screenshotSchemaTail+`},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			timeoutMs := int64(60_000)
			if value, ok := argInt(args, "timeoutMs"); ok {
				timeoutMs = value
			}
			client, err := s.getClient("hdmi_screenshot")
			if err != nil {
				return nil, err
			}
			image, err := client.CaptureHdmiScreenshot(ctx, timeoutMs)
			if err != nil {
				return nil, err
			}
			return imageCallResult([][]byte{image}, map[string]any{
				"success": true, "source": "hdmi", "format": "png", "bytes": len(image),
			})
		})

	register(s.mcp, "hdmi_ocr",
		"Capture one current screenshot from the HDMI-connected controlled machine and convert it to text on the controller machine. The ArkKVM device only supplies the H.264 HDMI video; OCR is performed locally using the command configured by ARKKVM_OCR_COMMAND, which defaults to tesseract. This does not inspect the controller desktop or use a direct connection to the controlled machine.",
		`{"type":"object","properties":{`+
			screenshotSchemaTail+`,`+
			`"language":{"type":"string","minLength":1,"description":"OCR language data name, for example eng, chi_sim, or eng+chi_sim. Default is ARKKVM_OCR_LANG or eng+chi_sim."},`+
			`"psm":{"type":"integer","minimum":0,"maximum":13,"description":"Tesseract page segmentation mode. Default is 6."}`+
			`},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			timeoutMs := int64(60_000)
			if value, ok := argInt(args, "timeoutMs"); ok {
				timeoutMs = value
			}
			language, _ := argString(args, "language")
			var psm *int
			if value, ok := argInt(args, "psm"); ok {
				v := int(value)
				psm = &v
			}
			client, err := s.getClient("hdmi_ocr")
			if err != nil {
				return nil, err
			}
			image, err := client.CaptureHdmiScreenshot(ctx, timeoutMs)
			if err != nil {
				return nil, err
			}
			result, err := ocr.RecognizePNG(image, ocrOptions(language, psm))
			if err != nil {
				return nil, err
			}
			return resultCallResult(map[string]any{
				"source": "hdmi", "format": "text", "text": result.Text,
				"language": result.Language, "psm": result.PSM,
			})
		})

	register(s.mcp, "hdmi_ocr_stream",
		"Sample the HDMI video for a short time and convert each sampled frame to text on the controller machine. The returned events contain only text changes, in chronological order. This is a bounded MCP operation, not a long-lived network stream; use durationMs and intervalMs to control its sampling window.",
		`{"type":"object","properties":{`+
			`"durationMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"Total OCR sampling window in milliseconds. Default is 5000."},`+
			`"intervalMs":{"type":"integer","exclusiveMinimum":0,"maximum":30000,"description":"Delay between OCR samples in milliseconds. Default is 1000."},`+
			screenshotSchemaTail+`,`+
			`"language":{"type":"string","minLength":1,"description":"OCR language data name. Default is ARKKVM_OCR_LANG or eng+chi_sim."},`+
			`"psm":{"type":"integer","minimum":0,"maximum":13,"description":"Tesseract page segmentation mode. Default is 6."}`+
			`},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			durationMs := int64(5_000)
			if value, ok := argInt(args, "durationMs"); ok {
				durationMs = value
			}
			intervalMs := int64(1_000)
			if value, ok := argInt(args, "intervalMs"); ok {
				intervalMs = value
			}
			var timeoutMs int64
			if value, ok := argInt(args, "timeoutMs"); ok {
				timeoutMs = value
			}
			language, hasLanguage := argString(args, "language")
			var psm *int
			if value, ok := argInt(args, "psm"); ok {
				v := int(value)
				psm = &v
			}
			client, err := s.getClient("hdmi_ocr_stream")
			if err != nil {
				return nil, err
			}
			startedAt := time.Now()
			deadline := startedAt.Add(time.Duration(durationMs) * time.Millisecond)
			type ocrEvent struct {
				Text         string `json:"text"`
				CapturedAtMs int64  `json:"capturedAtMs"`
			}
			events := []ocrEvent{}
			previousText := ""
			for time.Now().Before(deadline) {
				budget := timeoutMs
				if budget == 0 {
					budget = 60_000
				}
				if remaining := time.Until(deadline).Milliseconds(); remaining < budget {
					budget = remaining
				}
				if budget < 1 {
					budget = 1
				}
				image, err := client.CaptureHdmiScreenshot(ctx, budget)
				if err != nil {
					return nil, err
				}
				result, err := ocr.RecognizePNG(image, ocrOptions(language, psm))
				if err != nil {
					return nil, err
				}
				if result.Text != "" && result.Text != previousText {
					previousText = result.Text
					events = append(events, ocrEvent{Text: result.Text, CapturedAtMs: time.Now().UnixMilli()})
				}
				remaining := time.Until(deadline).Milliseconds()
				if remaining <= 0 {
					break
				}
				sleepMs := intervalMs
				if remaining < sleepMs {
					sleepMs = remaining
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(sleepMs) * time.Millisecond):
				}
			}
			lang := defaultOCRLanguage()
			if hasLanguage {
				lang = language
			}
			psmValue := 6
			if psm != nil {
				psmValue = *psm
			}
			return resultCallResult(map[string]any{
				"source": "hdmi", "format": "text",
				"durationMs": time.Since(startedAt).Milliseconds(),
				"language":   lang, "psm": psmValue, "events": events,
			})
		})

	register(s.mcp, "hdmi_frame_compare",
		"Capture two HDMI screenshots separated by a delay and compare them with a model-free grayscale pixel-difference algorithm. Use this to measure whether the controlled screen changed after an input action. It returns both PNG frames, a normalized change score, mean grayscale difference, changed-pixel counts, and approximate changed regions. The result does not identify text, applications, dialogs, buttons, success, or failure; the calling AI agent may inspect the returned image. This tool consumes only the ArkKVM HDMI WebRTC video track and does not send HID input.",
		`{"type":"object","properties":{`+
			`"delayMs":{"type":"integer","minimum":0,"maximum":60000,"description":"Milliseconds between the two HDMI captures. Default is 500."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":60000,"description":"Maximum wait for each decoded HDMI frame. Default is 60000; the first capture in a fresh MCP process takes a few seconds while the device emits its next keyframe to the agent session."},`+
			`"threshold":{"type":"number","exclusiveMinimum":0,"maximum":1,"description":"Fraction of pixels in the selected region that must differ by at least 16 grayscale levels to report changed. Must be greater than 0; default is 0.03."},`+
			`"region":{"type":"object","properties":{"x":{"type":"integer","minimum":0},"y":{"type":"integer","minimum":0},"width":{"type":"integer","exclusiveMinimum":0},"height":{"type":"integer","exclusiveMinimum":0}},"required":["x","y","width","height"],"additionalProperties":false,"description":"Optional screen region in source HDMI pixels. Omit to compare the full frame."}`+
			`},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			delayMs := int64(500)
			if value, ok := argInt(args, "delayMs"); ok {
				delayMs = value
			}
			timeoutMs := int64(60_000)
			if value, ok := argInt(args, "timeoutMs"); ok {
				timeoutMs = value
			}
			threshold := 0.03
			if value, ok := argNumber(args, "threshold"); ok {
				threshold = value
			}
			region, err := parseRegion(args, "region")
			if err != nil {
				return nil, err
			}
			client, err := s.getClient("hdmi_frame_compare")
			if err != nil {
				return nil, err
			}
			before, after, comparison, err := client.CompareHdmiFrames(ctx, delayMs, timeoutMs, threshold, region)
			if err != nil {
				return nil, err
			}
			return imageCallResult([][]byte{before, after}, comparison)
		})

	register(s.mcp, "hdmi_wait_for_change",
		"Capture HDMI frames until the controlled screen objectively changes or the timeout expires. Comparison is model-free grayscale pixel difference over the full screen or an optional source-pixel region. Use after keyboard, mouse, or wheel input when the next action depends on a new screen state. It returns the latest PNG and objective metrics only; it does not perform OCR/VLM or decide what the change means.",
		hdmiWaitSchema(),
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			options, err := s.parseWaitOptions(req)
			if err != nil {
				return nil, err
			}
			client, err := s.getClient("hdmi_wait_for_change")
			if err != nil {
				return nil, err
			}
			elapsedMs, comparison, image, err := client.WaitForHdmiChange(ctx, options)
			if err != nil {
				return nil, err
			}
			payload := map[string]any{}
			for key, value := range comparisonMap(comparison) {
				payload[key] = value
			}
			payload["elapsedMs"] = elapsedMs
			return imageCallResult([][]byte{image}, payload)
		})

	register(s.mcp, "hdmi_wait_for_stable",
		"Capture HDMI frames until the controlled screen remains objectively stable for a configured number of consecutive samples or the timeout expires. Stability is determined by model-free grayscale pixel difference and is useful after a page load or transition before sending the next HID action. It returns the latest PNG and objective metrics only; it does not determine whether the displayed task succeeded.",
		hdmiWaitSchema(),
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			options, err := s.parseWaitOptions(req)
			if err != nil {
				return nil, err
			}
			client, err := s.getClient("hdmi_wait_for_stable")
			if err != nil {
				return nil, err
			}
			stable, elapsedMs, comparison, image, err := client.WaitForHdmiStable(ctx, options)
			if err != nil {
				return nil, err
			}
			payload := map[string]any{}
			for key, value := range comparisonMap(comparison) {
				payload[key] = value
			}
			payload["stable"] = stable
			payload["elapsedMs"] = elapsedMs
			return imageCallResult([][]byte{image}, payload)
		})

	register(s.mcp, "hdmi_video_monitor",
		"Monitor the HDMI video stream continuously for a short period using one persistent H.264 decoder. This is the high-frequency, model-free alternative to repeated hdmi_screenshot calls: it consumes decoded grayscale frames at 160x90, compares consecutive frames, and returns frame counts, changed-frame counts, maximum/last change scores, and approximate changed regions. It does not return text or identify UI meaning; use hdmi_screenshot when the AI agent needs a full PNG for visual reasoning. Use this after an input action when timing and repeated screen-change sampling matter.",
		`{"type":"object","properties":{`+
			`"durationMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"How long to monitor the continuous HDMI stream. Default is 5000 milliseconds."},`+
			`"threshold":{"type":"number","exclusiveMinimum":0,"maximum":1,"description":"Fraction of 160x90 sample pixels that must differ by at least 16 grayscale levels to count a changed frame. Default is 0.03."},`+
			`"pixelThreshold":{"type":"integer","minimum":0,"maximum":255,"description":"Per-pixel grayscale difference threshold. Default is 16."},`+
			`"region":`+hdmiRegionSchema+`,`+
			`"maxEvents":{"type":"integer","exclusiveMinimum":0,"maximum":100,"description":"Maximum number of changed-region entries to return. Default is 20."}`+
			`},"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			durationMs := int64(5_000)
			if value, ok := argInt(args, "durationMs"); ok {
				durationMs = value
			}
			threshold := 0.03
			if value, ok := argNumber(args, "threshold"); ok {
				threshold = value
			}
			pixelThreshold := 16
			if value, ok := argInt(args, "pixelThreshold"); ok {
				pixelThreshold = int(value)
			}
			region, err := parseRegion(args, "region")
			if err != nil {
				return nil, err
			}
			maxEvents := int64(20)
			if value, ok := argInt(args, "maxEvents"); ok {
				maxEvents = value
			}
			client, err := s.getClient("hdmi_video_monitor")
			if err != nil {
				return nil, err
			}
			source, err := client.CreateHdmiFrameSource(ctx, 15_000)
			if err != nil {
				return nil, err
			}
			defer client.ReleaseHdmiFrameSource(source)
			// A fresh H.264 decoder must wait for the next keyframe the
			// device emits to agent sessions (~1 per second). Warm the
			// decoder up before the measurement window so durationMs counts
			// only the monitored period.
			if _, err := source.Next(60_000); err != nil {
				return nil, err
			}
			analysis, err := media.AnalyzeFrameStream(source, durationMs, media.CompareOptions{
				Threshold: threshold, PixelThreshold: pixelThreshold, Region: region,
			}, int(maxEvents))
			if err != nil {
				return nil, err
			}
			return resultCallResult(map[string]any{
				"source": "hdmi", "format": "gray8",
				"sampleSize": fmt.Sprintf("%dx%d", source.Width(), source.Height()),
				"durationMs": analysis.DurationMs, "frames": analysis.Frames,
				"comparisons": analysis.Comparisons, "changedFrames": analysis.ChangedFrames,
				"maxScore": analysis.MaxScore, "lastScore": analysis.LastScore,
				"changedRegions": analysis.ChangedRegions,
			})
		})

	register(s.mcp, "hdmi_wait_for_text",
		"Capture HDMI screenshots in a bounded loop, OCR each frame locally, and return as soon as the literal text appears on the controlled screen. Use this to wait for installer pages, boot menus, or login prompts instead of polling hdmi_ocr manually. Matching is whitespace-normalized literal substring matching, optionally case-insensitive (default); there is no regex or semantic understanding, and OCR errors can produce false negatives, so fall back to hdmi_screenshot for visual verification when this reports matched=false.",
		`{"type":"object","properties":{`+
			`"text":{"type":"string","minLength":1,"maxLength":256,"description":"Literal text to wait for, for example \"Install\" or \"Press ENTER to continue\". Whitespace runs are normalized on both sides."},`+
			`"caseSensitive":{"type":"boolean","description":"Match case-sensitively. Default false."},`+
			`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"Maximum total wait time in milliseconds. Default 30000."},`+
			`"intervalMs":{"type":"integer","exclusiveMinimum":0,"maximum":10000,"description":"Delay between capture+OCR samples in milliseconds. Default 1000."},`+
			`"screenshotTimeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":60000,"description":"Maximum wait for each decoded HDMI frame in milliseconds. Default 15000."},`+
			`"language":{"type":"string","minLength":1,"description":"OCR language data name. Default ARKKVM_OCR_LANG or eng+chi_sim."},`+
			`"psm":{"type":"integer","minimum":0,"maximum":13,"description":"Tesseract page segmentation mode. Default 6."}`+
			`},"required":["text"],"additionalProperties":false}`,
		func(ctx context.Context, req *callToolRequest) (*callToolResult, error) {
			args, err := decodeArgs(req.Params.Arguments)
			if err != nil {
				return nil, err
			}
			text, ok := argString(args, "text")
			if !ok || text == "" {
				return nil, fmt.Errorf("hdmi_wait_for_text requires text")
			}
			caseSensitive, _ := argBool(args, "caseSensitive")
			timeoutMs := int64(30_000)
			if value, has := argInt(args, "timeoutMs"); has {
				timeoutMs = value
			}
			intervalMs := int64(1_000)
			if value, has := argInt(args, "intervalMs"); has {
				intervalMs = value
			}
			var screenshotTimeoutMs int64
			if value, has := argInt(args, "screenshotTimeoutMs"); has {
				screenshotTimeoutMs = value
			}
			language, hasLanguage := argString(args, "language")
			var psm *int
			if value, has := argInt(args, "psm"); has {
				v := int(value)
				psm = &v
			}
			client, err := s.getClient("hdmi_wait_for_text")
			if err != nil {
				return nil, err
			}
			startedAt := time.Now()
			deadline := startedAt.Add(time.Duration(timeoutMs) * time.Millisecond)
			lastText := ""
			var lastImage []byte
			lastCaptureError := ""
			for {
				remaining := time.Until(deadline).Milliseconds()
				if remaining <= 0 {
					break
				}
				budget := screenshotTimeoutMs
				if budget == 0 {
					budget = 60_000
				}
				if remaining < budget {
					budget = remaining
				}
				if budget < 1 {
					budget = 1
				}
				image, captureErr := client.CaptureHdmiScreenshot(ctx, budget)
				if captureErr != nil {
					// During controlled-machine reboots the HDMI signal drops
					// and captures fail transiently; keep sampling until the
					// deadline instead of aborting.
					lastCaptureError = captureErr.Error()
				} else {
					lastImage = image
					lastCaptureError = ""
					result, ocrErr := ocr.RecognizePNG(image, ocrOptions(language, psm))
					if ocrErr != nil {
						lastCaptureError = ocrErr.Error()
					} else {
						lastText = result.Text
						if textmatch.ScreenTextIncludes(result.Text, text, caseSensitive) {
							lang := defaultOCRLanguage()
							if hasLanguage {
								lang = language
							}
							psmValue := 6
							if psm != nil {
								psmValue = *psm
							}
							payload := map[string]any{
								"matched":   true,
								"elapsedMs": time.Since(startedAt).Milliseconds(),
								"text":      lastText,
								"language":  lang,
								"psm":       psmValue,
							}
							return imageCallResult([][]byte{image}, payload)
						}
					}
				}
				nextRemaining := time.Until(deadline).Milliseconds()
				if nextRemaining <= 0 {
					break
				}
				sleepMs := intervalMs
				if nextRemaining < sleepMs {
					sleepMs = nextRemaining
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(sleepMs) * time.Millisecond):
				}
			}
			lang := defaultOCRLanguage()
			if hasLanguage {
				lang = language
			}
			psmValue := 6
			if psm != nil {
				psmValue = *psm
			}
			payload := map[string]any{
				"matched":   false,
				"elapsedMs": time.Since(startedAt).Milliseconds(),
				"text":      lastText,
				"language":  lang,
				"psm":       psmValue,
			}
			if lastCaptureError != "" {
				payload["lastCaptureError"] = lastCaptureError
			}
			images := [][]byte{}
			if lastImage != nil {
				images = append(images, lastImage)
			}
			return imageCallResult(images, payload)
		})
}

func hdmiWaitSchema() string {
	return `{"type":"object","properties":{` +
		`"timeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":120000,"description":"Maximum total time to wait. Default is 10000."},` +
		`"intervalMs":{"type":"integer","exclusiveMinimum":0,"maximum":10000,"description":"Time between HDMI samples. Default is 300."},` +
		`"stableSamples":{"type":"integer","exclusiveMinimum":0,"maximum":20,"description":"Number of consecutive unchanged comparisons required. Default is 3."},` +
		`"screenshotTimeoutMs":{"type":"integer","exclusiveMinimum":0,"maximum":60000,"description":"Maximum wait for each decoded frame. Default is 60000; the first capture in a fresh MCP process takes a few seconds while the device emits its next keyframe to the agent session."},` +
		`"threshold":{"type":"number","exclusiveMinimum":0,"maximum":1,"description":"Fraction of pixels in the selected region that must differ by at least 16 grayscale levels. Must be greater than 0; default is 0.03."},` +
		`"region":` + hdmiRegionSchema +
		`},"additionalProperties":false}`
}

func (s *Server) parseWaitOptions(req *callToolRequest) (device.WaitForHdmiOptions, error) {
	options := device.WaitForHdmiOptions{}
	args, err := decodeArgs(req.Params.Arguments)
	if err != nil {
		return options, err
	}
	if value, ok := argInt(args, "timeoutMs"); ok {
		options.TimeoutMs = value
	}
	if value, ok := argInt(args, "intervalMs"); ok {
		options.IntervalMs = value
	}
	if value, ok := argInt(args, "stableSamples"); ok {
		options.StableSamples = int(value)
	}
	if value, ok := argInt(args, "screenshotTimeoutMs"); ok {
		options.ScreenshotTimeoutMs = value
	}
	if value, ok := argNumber(args, "threshold"); ok {
		options.Threshold = value
	}
	region, err := parseRegion(args, "region")
	if err != nil {
		return options, err
	}
	options.Region = region
	return options, nil
}

func comparisonMap(comparison media.FrameComparison) map[string]any {
	data, err := json.Marshal(comparison)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}
