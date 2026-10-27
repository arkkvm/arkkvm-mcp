package settings

import "encoding/json"

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func mustUnmarshal(data []byte) any {
	var value any
	_ = json.Unmarshal(data, &value)
	return value
}
