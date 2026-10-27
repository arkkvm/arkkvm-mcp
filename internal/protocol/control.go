package protocol

// ControlPending is the dual-end arbitration request carried by getControlState
// / controlConflict / controlChanged.
type ControlPending struct {
	RequestID     string `json:"requestId"`
	Requester     string `json:"requester"`
	Reason        string `json:"reason"`
	DeadlineMs    int64  `json:"deadlineMs"`
	TimeoutPolicy string `json:"timeoutPolicy"`
}

// ControlState is the device HID control-lease snapshot (owner is the
// clientType that currently holds input, or null when vacant).
type ControlState struct {
	Owner   *string         `json:"owner"`
	Pending *ControlPending `json:"pending"`
	Reason  string          `json:"reason,omitempty"`
}
