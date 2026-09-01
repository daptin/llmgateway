package contract

import "encoding/json"

type AudioSpeechRequest struct {
	Input          string
	Voice          string
	Instructions   string
	Speed          *float64
	ResponseFormat string
}

type AudioSpeechResponse struct {
	Data        []byte
	ContentType string
}

type AudioTranscriptionRequest struct {
	File                   MediaFile
	Language               string
	Prompt                 string
	Temperature            *float64
	ResponseFormat         string
	TimestampGranularities []string
}

type MediaFile struct {
	Name        string
	ContentType string
	Data        []byte
}

type AudioTranscriptionResponse struct {
	Format string
	Text   string
	JSON   json.RawMessage
	Usage  Usage
}
