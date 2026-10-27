// Package upload holds pure parameters and limits for the storage-image
// upload pump implemented in the device package. Protocol notes:
//
//   - startStorageFileUpload {filename, size} returns
//     {alreadyUploadedBytes, dataChannel: "upload_<uuid>"}.
//   - The client sends raw binary chunks on that ordered DataChannel.
//   - The device throttles {"Size":N,"AlreadyUploadedBytes":M} progress
//     messages to ~200 ms and also emits that message on the same interval
//     when no new chunk has arrived, so a paused sender still learns how far
//     the write has gotten. A final progress message is sent when the byte
//     count reaches the size. The peer closes the channel.
//   - The device finalizes (renames <file>.incomplete to <file>) when the
//     channel closes with all bytes received; an early close keeps the
//     partial file so the same filename+size upload resumes.
package upload

import "math"

// ChunkSize is the per-send() chunk size. 16 KiB works reliably; 64 KiB
// messages get the upload DataChannel aborted by the device (verified against
// a live unit), so do not raise this without retesting.
const ChunkSize = 16 * 1024

// HighWatermark is the bufferedAmount pause threshold for the upload channel.
const HighWatermark = 512 * 1024

// LowWatermark is the bufferedAmount resume threshold for the upload channel.
const LowWatermark = 128 * 1024

// MaxInFlightChunks is how many chunks may be sent ahead of
// AlreadyUploadedBytes. The device drops a chunk when its per-channel event
// queue (256 slots) is full. 64 stays under that even if every in-flight
// chunk is still sitting in the queue.
const MaxInFlightChunks int64 = 64

// MaxInFlightBytes is MaxInFlightChunks of ChunkSize (1 MiB at 16 KiB).
const MaxInFlightBytes int64 = MaxInFlightChunks * ChunkSize

// DefaultTimeoutMs is the default wall-clock budget for one
// kvm_storage_upload tool call.
const DefaultTimeoutMs = 30 * 60_000

// MaxBytes is the hard cap on the file size the MCP is willing to stream to
// the device.
const MaxBytes int64 = 16 * 1024 * 1024 * 1024

const (
	ackStallBaseMs        = 60_000
	ackStallPerMegabyteMs = 1_000
	ackStallMaxMs         = 10 * 60_000
)

// ComputeAckStallTimeout is how long the final-acknowledgement wait may go
// without progress before the upload is considered stalled. Scales with file
// size so large ISOs on slow links are not aborted by a temporarily quiet
// channel: 60 s + 1 s per MB, capped at 10 minutes. Mirrors the frontend ACK
// stall policy.
func ComputeAckStallTimeout(totalBytes int64) int64 {
	extraMs := (float64(totalBytes) / (1024 * 1024)) * ackStallPerMegabyteMs
	return int64(math.Min(ackStallBaseMs+extraMs, ackStallMaxMs))
}
