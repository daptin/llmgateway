package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const maxBatchLines = 50000

type BatchInputLine struct {
	Line     int64
	CustomID string
	Method   string
	URL      string
	Body     json.RawMessage
}

type BatchOutputLine struct {
	ID         string
	CustomID   string
	StatusCode int
	RequestID  string
	Body       json.RawMessage
	Error      json.RawMessage
}

func DecodeBatchInput(data []byte, endpoint string) ([]BatchInputLine, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	maximumLine := len(data) + 1
	if maximumLine < 64*1024 {
		maximumLine = 64 * 1024
	}
	scanner.Buffer(make([]byte, 64*1024), maximumLine)
	seen := make(map[string]bool)
	lines := make([]BatchInputLine, 0)
	var physicalLine int64
	for scanner.Scan() {
		physicalLine++
		encoded := bytes.TrimSpace(scanner.Bytes())
		if len(encoded) == 0 {
			continue
		}
		if len(lines) >= maxBatchLines {
			return nil, fmt.Errorf("batch exceeds %d requests", maxBatchLines)
		}
		if err := rejectDuplicateJSONKeys(encoded); err != nil {
			return nil, fmt.Errorf("batch line %d contains invalid or duplicate JSON fields: %w", physicalLine, err)
		}
		var wire struct {
			CustomID string          `json:"custom_id"`
			Method   string          `json:"method"`
			URL      string          `json:"url"`
			Body     json.RawMessage `json:"body"`
		}
		if err := decodeStrict(encoded, &wire); err != nil {
			return nil, fmt.Errorf("batch line %d is invalid: %w", physicalLine, err)
		}
		wire.CustomID = strings.TrimSpace(wire.CustomID)
		wire.Method = strings.ToUpper(strings.TrimSpace(wire.Method))
		wire.URL = strings.TrimSpace(wire.URL)
		if wire.CustomID == "" || len(wire.CustomID) > 200 || seen[wire.CustomID] {
			return nil, fmt.Errorf("batch line %d has an invalid or duplicate custom_id", physicalLine)
		}
		if wire.Method != "POST" || !validBatchEndpoint(wire.URL) || (endpoint != "" && wire.URL != endpoint) {
			return nil, fmt.Errorf("batch line %d has an unsupported method or URL", physicalLine)
		}
		body := bytes.TrimSpace(wire.Body)
		if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
			return nil, fmt.Errorf("batch line %d body must be a JSON object", physicalLine)
		}
		if err := rejectDuplicateJSONKeys(body); err != nil {
			return nil, fmt.Errorf("batch line %d body contains invalid or duplicate JSON fields: %w", physicalLine, err)
		}
		var controls struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &controls); err != nil {
			return nil, fmt.Errorf("batch line %d body is invalid: %w", physicalLine, err)
		}
		if controls.Stream {
			return nil, fmt.Errorf("batch line %d cannot request streaming", physicalLine)
		}
		seen[wire.CustomID] = true
		lines = append(lines, BatchInputLine{Line: physicalLine, CustomID: wire.CustomID, Method: wire.Method, URL: wire.URL,
			Body: append(json.RawMessage(nil), body...)})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read batch input: %w", err)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("batch input is empty")
	}
	return lines, nil
}

func EncodeBatchOutput(lines []BatchOutputLine) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, line := range lines {
		if line.ID == "" || line.CustomID == "" {
			return nil, fmt.Errorf("batch output identifiers are required")
		}
		value := map[string]any{"id": line.ID, "custom_id": line.CustomID, "error": nil}
		if len(line.Error) > 0 {
			if !json.Valid(line.Error) {
				return nil, fmt.Errorf("batch output error is invalid JSON")
			}
			value["response"] = nil
			value["error"] = line.Error
		} else {
			if line.StatusCode < 100 || line.StatusCode > 599 || !json.Valid(line.Body) {
				return nil, fmt.Errorf("batch output response is invalid")
			}
			value["response"] = map[string]any{"status_code": line.StatusCode, "request_id": line.RequestID, "body": line.Body}
		}
		if err := encoder.Encode(value); err != nil {
			return nil, fmt.Errorf("encode batch output: %w", err)
		}
	}
	return output.Bytes(), nil
}
