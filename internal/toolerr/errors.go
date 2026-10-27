package toolerr

import (
	"errors"
	"strconv"
)

// AsDeviceAuthError and AsConnectionError wrap errors.As for the two
// connection-plane error types.
func AsDeviceAuthError(err error, target **DeviceAuthError) bool {
	return errors.As(err, target)
}

func AsConnectionError(err error, target **ConnectionError) bool {
	return errors.As(err, target)
}

func parseNumeric(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
