package jsonx

import (
	"bytes"
	"testing"
)

func FuzzDecodeOneNeverAcceptsTrailingDocument(f *testing.F) {
	for _, seed := range []string{
		`{"name":"value"}`,
		`{"name":"value"} {"name":"second"}`,
		`{"unknown":true}`,
		`{`,
	} {
		f.Add([]byte(seed))
	}
	type document struct {
		Name string `json:"name"`
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 1<<20 {
			return
		}
		var decoded document
		if err := DecodeOne(bytes.NewReader(payload), &decoded); err != nil {
			return
		}
		combined := make([]byte, 0, len(payload)+18)
		combined = append(combined, payload...)
		combined = append(combined, []byte(` {"name":"second"}`)...)
		if err := DecodeOne(bytes.NewReader(combined), &decoded); err == nil {
			t.Fatal("accepted a trailing JSON document")
		}
	})
}
