package upload

import "testing"

func TestComputeAckStallTimeout(t *testing.T) {
	cases := []struct {
		total int64
		want  int64
	}{
		{0, 60_000},
		{1024 * 1024, 61_000},
		{10 * 1024 * 1024, 70_000},
		{600 * 1024 * 1024, 600_000}, // 660s request capped at 10 min
		// Capped at 10 minutes.
		{16 * 1024 * 1024 * 1024, 600_000},
	}
	for _, c := range cases {
		if got := ComputeAckStallTimeout(c.total); got != c.want {
			t.Errorf("ComputeAckStallTimeout(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

func TestConstants(t *testing.T) {
	if ChunkSize != 16*1024 {
		t.Error("chunk size must stay 16KiB: 64KiB gets the channel aborted by the device")
	}
	if HighWatermark != 512*1024 || LowWatermark != 128*1024 {
		t.Error("watermarks drifted")
	}
	if MaxInFlightChunks != 64 || MaxInFlightBytes != 64*ChunkSize {
		t.Errorf("in-flight window = %d chunks / %d bytes", MaxInFlightChunks, MaxInFlightBytes)
	}
	if MaxInFlightChunks >= 256 {
		t.Error("in-flight chunks must stay under the device DataChannel queue of 256")
	}
	if MaxBytes != 16*1024*1024*1024 {
		t.Error("max upload size drifted")
	}
}
