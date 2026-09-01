package openai

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestAudioSpeechProtocolPreservesControlsAndBinaryResponse(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{AudioSpeech: &contract.AudioSpeechResponse{
		Data: []byte{0, 1, 2, 3}, ContentType: "audio/mpeg",
	}}}
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/audio/speech",
		bytes.NewBufferString(`{"model":"allowed","input":"hello","voice":"alloy","instructions":"calm","speed":1.25}`)))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status=%d body=%s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewBufferString(`{"model":"allowed","input":"hello","voice":"alloy","instructions":"calm","speed":1.25}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer key")
	request.Header.Set("X-Request-ID", "req_test")
	response = httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "audio/mpeg" || !bytes.Equal(response.Body.Bytes(), []byte{0, 1, 2, 3}) {
		t.Fatalf("speech status=%d content-type=%q body=%v", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
	}
	if engine.invokeRequest.AudioSpeech == nil || engine.invokeRequest.AudioSpeech.ResponseFormat != "mp3" || engine.invokeRequest.AudioSpeech.Speed == nil || *engine.invokeRequest.AudioSpeech.Speed != 1.25 {
		t.Fatalf("canonical speech request=%#v", engine.invokeRequest)
	}
	if engine.invokeRequest.EstimatedUsage.InputTokens != 9 || engine.invokeRequest.EstimatedUsage.Measures["request_bytes"] == 0 {
		t.Fatalf("speech usage estimate=%#v", engine.invokeRequest.EstimatedUsage)
	}
}

func TestAudioTranscriptionAndTranslationProtocol(t *testing.T) {
	for _, test := range []struct {
		path      string
		operation contract.Operation
		format    string
		result    contract.AudioTranscriptionResponse
		content   string
	}{
		{path: "/v1/audio/transcriptions", operation: contract.OperationTranscription, format: "verbose_json",
			result: contract.AudioTranscriptionResponse{Format: "verbose_json", Text: "hello", JSON: []byte(`{"text":"hello","language":"en"}`)}, content: `{"text":"hello","language":"en"}`},
		{path: "/v1/audio/translations", operation: contract.OperationTranslation, format: "text",
			result: contract.AudioTranscriptionResponse{Format: "text", Text: "translated"}, content: "translated"},
	} {
		t.Run(string(test.operation), func(t *testing.T) {
			engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Transcription: &test.result}}
			fields := map[string][]string{"model": {" allowed "}, "prompt": {"context"}, "temperature": {"0.2"}, "response_format": {test.format}}
			if test.operation == contract.OperationTranscription {
				fields["language"] = []string{"en"}
				fields["timestamp_granularities[]"] = []string{"word", "segment"}
			}
			response := httptest.NewRecorder()
			testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, multipartRequest(t, test.path, fields, "sample.wav", []byte{1, 2, 3}))
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if test.format == "verbose_json" {
				assertJSONEqual(t, response.Body.String(), test.content)
			} else if response.Body.String() != test.content {
				t.Fatalf("body=%q want=%q", response.Body.String(), test.content)
			}
			canonical := engine.invokeRequest
			if canonical.Operation != test.operation || canonical.PublicModel != "allowed" || canonical.Transcription == nil || canonical.Transcription.File.Name != "sample.wav" || canonical.Transcription.Prompt != "context" {
				t.Fatalf("canonical audio request=%#v", canonical)
			}
			if canonical.EstimatedUsage.TotalTokens != 0 || canonical.EstimatedUsage.Measures["audio_bytes"] != 3 {
				t.Fatalf("audio usage estimate=%#v", canonical.EstimatedUsage)
			}
		})
	}
}

func TestAudioMultipartRejectsDuplicateAndOversizeInput(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t)}
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, multipartRequest(t, "/v1/audio/transcriptions",
		map[string][]string{"model": {"allowed", "other"}}, "sample.wav", []byte{1}))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate model status=%d body=%s", response.Code, response.Body.String())
	}
	handler, err := NewHandler(engine, fakeAuthenticator{}, Options{MaxBodyBytes: 64, NewRequestID: func() (contract.ID, error) { return "req", nil }})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, multipartRequest(t, "/v1/audio/transcriptions", map[string][]string{"model": {"allowed"}}, "sample.wav", bytes.Repeat([]byte{1}, 128)))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d body=%s", response.Code, response.Body.String())
	}
}
