package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestInvokeAudioSpeechUsesSharedTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/audio/speech" || request.Header.Get("Authorization") != "Bearer secret-key" || request.Header.Get("Accept") != "audio/wav" || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected upstream speech request: path=%s headers=%v", request.URL.Path, request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "upstream-model" || body["input"] != "hello" || body["voice"] != "alloy" || body["response_format"] != "wav" || body["instructions"] != "calm" {
			t.Fatalf("unexpected upstream speech body: %#v", body)
		}
		response.Header().Set("Content-Type", "audio/wav")
		_, _ = response.Write([]byte{1, 2, 3})
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	result, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationAudioSpeech,
		AudioSpeech: &contract.AudioSpeechRequest{Input: "hello", Voice: "alloy", Instructions: "calm", ResponseFormat: "wav"}})
	if err != nil || result.AudioSpeech == nil || result.AudioSpeech.ContentType != "audio/wav" || len(result.AudioSpeech.Data) != 3 {
		t.Fatalf("speech result=%#v err=%v", result, err)
	}
}

func TestInvokeAudioMultipartPreservesFileControlsAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/audio/transcriptions" || !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Fatalf("unexpected upstream transcription request: path=%s content-type=%q", request.URL.Path, request.Header.Get("Content-Type"))
		}
		if err := request.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if header.Filename != "sample.wav" || string(data) != "audio" || len(request.MultipartForm.Value["model"]) != 1 || request.FormValue("model") != "upstream-model" ||
			request.FormValue("language") != "en" || request.FormValue("prompt") != "context" || request.FormValue("response_format") != "verbose_json" ||
			request.FormValue("vendor_flag") != "enabled" || len(request.MultipartForm.Value["timestamp_granularities[]"]) != 2 {
			t.Fatalf("unexpected multipart form: values=%v filename=%q data=%q", request.MultipartForm.Value, header.Filename, data)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"text":"hello","language":"en","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	temperature := 0.2
	result, err := adapter.Invoke(context.Background(), deploymentWithParameters(`{"audio_transcription":{"vendor_flag":"enabled"}}`), contract.Request{
		Operation: contract.OperationTranscription, Transcription: &contract.AudioTranscriptionRequest{
			File: contract.MediaFile{Name: "sample.wav", ContentType: "audio/wav", Data: []byte("audio")}, Language: "en", Prompt: "context",
			Temperature: &temperature, ResponseFormat: "verbose_json", TimestampGranularities: []string{"word", "segment"},
		}})
	if err != nil || result.Transcription == nil || result.Transcription.Text != "hello" || result.Usage.TotalTokens != 9 || !json.Valid(result.Transcription.JSON) {
		t.Fatalf("transcription result=%#v err=%v", result, err)
	}
}

func TestAudioAdapterRejectsWrongProviderMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"error":"not audio"}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	_, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationAudioSpeech,
		AudioSpeech: &contract.AudioSpeechRequest{Input: "hello", Voice: "alloy", ResponseFormat: "mp3"}})
	if err == nil {
		t.Fatal("accepted non-audio speech response")
	}
}
