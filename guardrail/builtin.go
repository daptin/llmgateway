package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
)

type PhraseFactory struct{}

type phraseConfig struct {
	Mode          string   `json:"mode"`
	Patterns      []string `json:"patterns"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
}

type phraseChecker struct{ config phraseConfig }

func (PhraseFactory) Build(configuration catalog.Guardrail) (Checker, error) {
	var config phraseConfig
	if err := strictConfig(configuration.Config, &config); err != nil {
		return nil, err
	}
	if err := validateRuleConfig(config.Mode, config.Patterns); err != nil {
		return nil, err
	}
	if !config.CaseSensitive {
		for index := range config.Patterns {
			config.Patterns[index] = strings.ToLower(config.Patterns[index])
		}
	}
	return phraseChecker{config: config}, nil
}

func (c phraseChecker) CheckInput(_ context.Context, request contract.Request) (Decision, error) {
	return c.decide(requestText(request)), nil
}

func (c phraseChecker) CheckOutput(_ context.Context, _ contract.Request, response contract.Response) (Decision, error) {
	return c.decide(responseText(response)), nil
}

func (c phraseChecker) CheckStream(_ context.Context, _ contract.Request, event contract.StreamEvent) (Decision, error) {
	return c.decide(eventText(event)), nil
}

func (phraseChecker) SupportsStreaming() bool { return false }
func (phraseChecker) CacheStable() bool       { return true }

func (c phraseChecker) decide(text string) Decision {
	if !c.config.CaseSensitive {
		text = strings.ToLower(text)
	}
	matched := ""
	for _, pattern := range c.config.Patterns {
		if strings.Contains(text, pattern) {
			matched = pattern
			break
		}
	}
	allowed := matched == ""
	if c.config.Mode == "allow" {
		allowed = matched != ""
	}
	reason := ""
	if !allowed {
		reason = "phrase_policy"
	}
	return Decision{Allowed: allowed, Reason: reason}
}

type RegexFactory struct{}

type regexConfig struct {
	Mode     string   `json:"mode"`
	Patterns []string `json:"patterns"`
}

type regexChecker struct {
	mode     string
	patterns []*regexp.Regexp
}

func (RegexFactory) Build(configuration catalog.Guardrail) (Checker, error) {
	var config regexConfig
	if err := strictConfig(configuration.Config, &config); err != nil {
		return nil, err
	}
	if err := validateRuleConfig(config.Mode, config.Patterns); err != nil {
		return nil, err
	}
	checker := regexChecker{mode: config.Mode}
	for _, pattern := range config.Patterns {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile pattern: %w", err)
		}
		checker.patterns = append(checker.patterns, compiled)
	}
	return checker, nil
}

func (c regexChecker) CheckInput(_ context.Context, request contract.Request) (Decision, error) {
	return c.decide(requestText(request)), nil
}
func (c regexChecker) CheckOutput(_ context.Context, _ contract.Request, response contract.Response) (Decision, error) {
	return c.decide(responseText(response)), nil
}
func (c regexChecker) CheckStream(_ context.Context, _ contract.Request, event contract.StreamEvent) (Decision, error) {
	return c.decide(eventText(event)), nil
}
func (regexChecker) SupportsStreaming() bool { return false }
func (regexChecker) CacheStable() bool       { return true }

func (c regexChecker) decide(text string) Decision {
	matched := false
	for _, pattern := range c.patterns {
		if pattern.MatchString(text) {
			matched = true
			break
		}
	}
	allowed := !matched
	if c.mode == "allow" {
		allowed = matched
	}
	reason := ""
	if !allowed {
		reason = "regex_policy"
	}
	return Decision{Allowed: allowed, Reason: reason}
}

type BoundsFactory struct{}

type boundsConfig struct {
	MaxInputTokens  int64 `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

type boundsChecker struct{ config boundsConfig }

func (BoundsFactory) Build(configuration catalog.Guardrail) (Checker, error) {
	var config boundsConfig
	if err := strictConfig(configuration.Config, &config); err != nil {
		return nil, err
	}
	if config.MaxInputTokens < 0 || config.MaxOutputTokens < 0 || (config.MaxInputTokens == 0 && config.MaxOutputTokens == 0) {
		return nil, errors.New("bounds requires at least one positive token limit")
	}
	return boundsChecker{config: config}, nil
}

func (c boundsChecker) CheckInput(_ context.Context, request contract.Request) (Decision, error) {
	if c.config.MaxInputTokens > 0 && request.EstimatedUsage.InputTokens > c.config.MaxInputTokens {
		return Decision{Allowed: false, Reason: "input_token_bound"}, nil
	}
	if c.config.MaxOutputTokens > 0 && request.MaxOutputTokens > c.config.MaxOutputTokens {
		return Decision{Allowed: false, Reason: "output_token_bound"}, nil
	}
	return Decision{Allowed: true}, nil
}

func (boundsChecker) CheckOutput(context.Context, contract.Request, contract.Response) (Decision, error) {
	return Decision{Allowed: true}, nil
}
func (boundsChecker) CheckStream(context.Context, contract.Request, contract.StreamEvent) (Decision, error) {
	return Decision{Allowed: true}, nil
}
func (boundsChecker) SupportsStreaming() bool { return true }
func (boundsChecker) CacheStable() bool       { return true }

func validateRuleConfig(mode string, patterns []string) error {
	if mode != "allow" && mode != "deny" {
		return errors.New("rule mode must be allow or deny")
	}
	if len(patterns) == 0 {
		return errors.New("at least one pattern is required")
	}
	for _, pattern := range patterns {
		if pattern == "" {
			return errors.New("patterns cannot be empty")
		}
	}
	return nil
}

func strictConfig(raw json.RawMessage, destination any) error {
	return jsonx.DecodeOne(bytes.NewReader(raw), destination)
}

func requestText(request contract.Request) string {
	var builder strings.Builder
	if request.Chat != nil {
		appendMessages(&builder, request.Chat.Messages)
		appendTools(&builder, request.Chat.Tools)
	}
	if request.TextCompletion != nil {
		for _, text := range request.TextCompletion.Prompt.Texts {
			builder.WriteString(text)
			builder.WriteByte('\n')
		}
		builder.WriteString(request.TextCompletion.Suffix)
	}
	if request.Responses != nil {
		builder.WriteString(request.Responses.Instructions)
		for _, item := range request.Responses.Input {
			appendParts(&builder, item.Content)
			builder.WriteString(item.Name)
			builder.WriteString(item.Arguments)
			builder.WriteString(item.Output)
		}
		appendTools(&builder, request.Responses.Tools)
	}
	if request.Embeddings != nil {
		for _, text := range request.Embeddings.Input.Texts {
			builder.WriteString(text)
			builder.WriteByte('\n')
		}
	}
	if request.ImageGeneration != nil {
		builder.WriteString(request.ImageGeneration.Prompt)
	}
	if request.ImageEdit != nil {
		builder.WriteString(request.ImageEdit.Prompt)
	}
	if request.Moderation != nil {
		for _, input := range request.Moderation.Input {
			builder.WriteString(input.Text)
			builder.WriteByte('\n')
		}
	}
	if request.Rerank != nil {
		builder.WriteString(request.Rerank.Query)
		builder.WriteString(request.Rerank.Instruction)
		for _, document := range request.Rerank.Documents {
			builder.WriteString(document.Text)
			builder.Write(document.Object)
			builder.WriteByte('\n')
		}
	}
	if request.AudioSpeech != nil {
		builder.WriteString(request.AudioSpeech.Input)
		builder.WriteString(request.AudioSpeech.Instructions)
	}
	if request.Transcription != nil {
		builder.WriteString(request.Transcription.Prompt)
	}
	if request.Search != nil {
		for _, query := range request.Search.Queries {
			builder.WriteString(query)
			builder.WriteByte('\n')
		}
	}
	if request.OCR != nil {
		builder.WriteString(request.OCR.DocumentAnnotationPrompt)
	}
	return builder.String()
}

func responseText(response contract.Response) string {
	var builder strings.Builder
	if response.Chat != nil {
		for _, choice := range response.Chat.Choices {
			appendMessages(&builder, []contract.Message{choice.Message})
		}
	}
	if response.TextCompletion != nil {
		for _, choice := range response.TextCompletion.Choices {
			builder.WriteString(choice.Text)
		}
	}
	if response.Responses != nil {
		for _, item := range response.Responses.Output {
			appendParts(&builder, item.Content)
			appendParts(&builder, item.Summary)
			builder.WriteString(item.Name)
			builder.WriteString(item.Arguments)
		}
	}
	if response.Images != nil {
		for _, image := range response.Images.Data {
			builder.WriteString(image.RevisedPrompt)
		}
	}
	if response.Rerank != nil {
		for _, result := range response.Rerank.Results {
			if result.Document != nil {
				builder.WriteString(result.Document.Text)
				builder.Write(result.Document.Object)
			}
		}
	}
	if response.Transcription != nil {
		builder.WriteString(response.Transcription.Text)
	}
	if response.Search != nil {
		for _, result := range response.Search.Results {
			builder.WriteString(result.Title)
			builder.WriteString(result.Snippet)
			builder.WriteByte('\n')
		}
	}
	if response.OCR != nil {
		builder.WriteString(response.OCR.Content)
		builder.Write(response.OCR.DocumentAnnotation)
		for _, page := range response.OCR.Pages {
			builder.WriteString(page.Markdown)
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func eventText(event contract.StreamEvent) string {
	var builder strings.Builder
	if event.Chat != nil {
		builder.WriteString(event.Chat.Content)
		for _, call := range event.Chat.ToolCalls {
			builder.WriteString(call.Function.Name)
			builder.WriteString(call.Function.Arguments)
		}
	}
	if event.TextCompletion != nil {
		builder.WriteString(event.TextCompletion.Text)
	}
	if event.Response != nil {
		builder.WriteString(event.Response.Delta)
		if event.Response.Item != nil {
			appendParts(&builder, event.Response.Item.Content)
			builder.WriteString(event.Response.Item.Name)
			builder.WriteString(event.Response.Item.Arguments)
		}
	}
	return builder.String()
}

func appendMessages(builder *strings.Builder, messages []contract.Message) {
	for _, message := range messages {
		appendParts(builder, message.Content)
		for _, call := range message.ToolCalls {
			builder.WriteString(call.Function.Name)
			builder.WriteString(call.Function.Arguments)
		}
	}
}

func appendParts(builder *strings.Builder, parts []contract.ContentPart) {
	for _, part := range parts {
		builder.WriteString(part.Text)
	}
}

func appendTools(builder *strings.Builder, tools []contract.Tool) {
	for _, tool := range tools {
		builder.WriteString(tool.Function.Name)
		builder.WriteString(tool.Function.Description)
		builder.Write(tool.Function.Parameters)
	}
}
