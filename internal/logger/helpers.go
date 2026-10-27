package logger

import (
	"encoding/json"
	"reflect"
)

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func errorName(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return "Error"
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		return "Error"
	}
	return name
}
