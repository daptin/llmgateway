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
		var decoded document
		if err := DecodeOne(bytes.NewReader(payload), &decoded); err != nil || len(payload) > 1<<20 {
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
