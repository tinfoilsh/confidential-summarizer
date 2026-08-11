package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/tinfoilsh/tinfoil-go"

	"github.com/tinfoilsh/confidential-summarizer/config"
)

const openAISDKMaxRetries = 0

type summaryInput struct {
	Content   string
	Style     config.Style
	MinWords  int
	MaxWords  int
	MaxTokens int
}

type summaryUpstream interface {
	Summarize(context.Context, summaryInput) (string, error)
}

type openAIUpstream struct {
	client *openai.Client
	model  string
}

type upstreamHTTPError struct {
	statusCode int
	response   *http.Response
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream returned HTTP status %d", e.statusCode)
}

func newTinfoilClient(apiKey string) (*tinfoil.Client, error) {
	return tinfoil.NewClient(openAIClientOptions(apiKey)...)
}

func openAIClientOptions(apiKey string) []option.RequestOption {
	return []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMaxRetries(openAISDKMaxRetries),
	}
}

func newOpenAIUpstream(client *openai.Client, model string) *openAIUpstream {
	return &openAIUpstream{client: client, model: model}
}

func (u *openAIUpstream) Summarize(ctx context.Context, input summaryInput) (string, error) {
	systemPrompt := fmt.Sprintf("%s Use between %d and %d words.", input.Style.SystemPrompt, input.MinWords, input.MaxWords)
	params := openai.ChatCompletionNewParams{
		Model:               u.model,
		MaxCompletionTokens: openai.Int(int64(input.MaxTokens)),
		ReasoningEffort:     openai.ReasoningEffortLow,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(input.Content),
		},
	}
	if input.Style.Temperature != nil {
		params.Temperature = openai.Float(*input.Style.Temperature)
	}

	var responseBody []byte
	var httpResponse *http.Response
	_, err := u.client.Chat.Completions.New(ctx, params,
		option.WithResponseBodyInto(&responseBody),
		option.WithResponseInto(&httpResponse),
	)
	if err != nil {
		var apiError *openai.Error
		if !errors.As(err, &apiError) && httpResponse != nil && httpResponse.StatusCode >= http.StatusBadRequest {
			return "", &upstreamHTTPError{statusCode: httpResponse.StatusCode, response: httpResponse}
		}
		return "", err
	}

	var response openai.ChatCompletion
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", err
	}
	if len(response.Choices) == 0 {
		return "", nil
	}
	return response.Choices[0].Message.Content, nil
}
