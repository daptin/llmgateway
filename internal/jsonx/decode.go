package jsonx

import (
	"encoding/json"
	"errors"
	"io"
)

// DecodeOne decodes exactly one JSON document and rejects unknown fields.
func DecodeOne(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("expected one JSON document")
		}
		return err
	}
	return nil
}
