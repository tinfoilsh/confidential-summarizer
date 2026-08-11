package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/sirupsen/logrus"
)

const (
	testContent       = "private content"
	testSummary       = "safe summary"
	testSecret        = "highly-sensitive-value"
	testTimeout       = 10 * time.Millisecond
	testHTTPErrorBody = `{"error":{"message":"unavailable","type":"server_error"}}`
)

type fakeUpstream struct {
	summarize func(context.Context, summaryInput) (string, error)
}

func (f fakeUpstream) Summarize(ctx context.Context, input summaryInput) (string, error) {
	return f.summarize(ctx, input)
}

func testService(upstream summaryUpstream, upstreamTimeout time.Duration, output io.Writer) *summaryService {
	logger := logrus.New()
	logger.SetOutput(output)
	logger.SetFormatter(&logrus.JSONFormatter{})
	return newSummaryService(upstream, upstreamTimeout, logger)
}

func performRequest(service http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/summarize", strings.NewReader(body))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	return response
}

func decodeErrorResponse(t *testing.T, response *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var body errorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	return body
}

func TestRequiredAPIKey(t *testing.T) {
	if _, err := requiredAPIKey(" \t "); err == nil {
		t.Fatal("expected whitespace API key to be rejected")
	}
	if key, err := requiredAPIKey(" key "); err != nil || key != "key" {
		t.Fatalf("expected API key to be accepted, key=%q err=%v", key, err)
	}
}

func TestRequestValidation(t *testing.T) {
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) {
		return testSummary, nil
	}}
	service := testService(upstream, time.Second, io.Discard)

	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "malformed JSON", body: `{`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "missing content", body: `{}`, status: http.StatusBadRequest, code: "invalid_content"},
		{name: "unknown style", body: `{"content":"text","style":"unknown"}`, status: http.StatusBadRequest, code: "invalid_style"},
		{name: "reversed words", body: `{"content":"text","min_words":20,"max_words":10}`, status: http.StatusBadRequest, code: "invalid_word_range"},
		{name: "word limit", body: `{"content":"text","max_words":1001}`, status: http.StatusBadRequest, code: "invalid_word_range"},
		{name: "token limit", body: `{"content":"text","max_tokens":32769}`, status: http.StatusBadRequest, code: "invalid_max_tokens"},
		{name: "unknown field", body: `{"content":"text","extra":true}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "oversized", body: fmt.Sprintf(`{"content":"%s"}`, strings.Repeat("a", maxRequestBodyBytes)), status: http.StatusRequestEntityTooLarge, code: "request_too_large"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRequest(service, test.body)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			body := decodeErrorResponse(t, response)
			if body.Code != test.code || body.RequestID == "" {
				t.Fatalf("error = %+v, want code %q and request ID", body, test.code)
			}
			if body.Error == "" {
				t.Fatalf("error response is missing an error: %+v", body)
			}
			if response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestValidatedOverridesReachUpstream(t *testing.T) {
	received := make(chan summaryInput, 1)
	upstream := fakeUpstream{summarize: func(_ context.Context, input summaryInput) (string, error) {
		received <- input
		return testSummary, nil
	}}
	service := testService(upstream, time.Second, io.Discard)
	response := performRequest(service, `{"content":"text","style":"title_summary","min_words":3,"max_words":8,"max_tokens":512}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	input := <-received
	if input.MinWords != 3 || input.MaxWords != 8 || input.MaxTokens != 512 {
		t.Fatalf("upstream input = %+v", input)
	}
}

func TestUpstreamErrorMappings(t *testing.T) {
	rateResponse := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"7"}}}
	tests := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{name: "rate limited", err: &openai.Error{StatusCode: http.StatusTooManyRequests, Response: rateResponse}, status: http.StatusTooManyRequests, code: "upstream_rate_limited", retryAfter: "7"},
		{name: "upstream unavailable", err: &openai.Error{StatusCode: http.StatusBadGateway}, status: http.StatusServiceUnavailable, code: "upstream_unavailable"},
		{name: "upstream rejection", err: &openai.Error{StatusCode: http.StatusBadRequest}, status: http.StatusBadGateway, code: "upstream_error"},
		{name: "malformed response", err: &json.SyntaxError{}, status: http.StatusBadGateway, code: "invalid_upstream_response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) { return "", test.err }}
			service := testService(upstream, time.Second, io.Discard)
			response := performRequest(service, `{"content":"text"}`)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			body := decodeErrorResponse(t, response)
			if body.Code != test.code {
				t.Fatalf("code = %q, want %q", body.Code, test.code)
			}
			if response.Header().Get("Retry-After") != test.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", response.Header().Get("Retry-After"), test.retryAfter)
			}
		})
	}
}

func TestOpenAIResponseClassificationUsesStatusAndJSONTypes(t *testing.T) {
	tests := []struct {
		name         string
		upstreamCode int
		serviceCode  int
		errorCode    string
	}{
		{name: "malformed success", upstreamCode: http.StatusOK, serviceCode: http.StatusBadGateway, errorCode: "invalid_upstream_response"},
		{name: "malformed server error", upstreamCode: http.StatusInternalServerError, serviceCode: http.StatusServiceUnavailable, errorCode: "upstream_unavailable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.upstreamCode)
				w.Write([]byte(`{`))
			}))
			defer server.Close()

			options := append(openAIClientOptions("test-key"), option.WithBaseURL(server.URL+"/"), option.WithHTTPClient(server.Client()))
			client := openai.NewClient(options...)
			service := testService(newOpenAIUpstream(&client, "gpt-oss-120b"), time.Second, io.Discard)
			response := performRequest(service, `{"content":"text"}`)
			body := decodeErrorResponse(t, response)
			if response.Code != test.serviceCode || body.Code != test.errorCode {
				t.Fatalf("response = %d %+v, want %d %q", response.Code, body, test.serviceCode, test.errorCode)
			}
		})
	}
}

func TestUpstreamTimeoutAndCancellationAreDistinct(t *testing.T) {
	upstream := fakeUpstream{summarize: func(ctx context.Context, _ summaryInput) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}}
	service := testService(upstream, testTimeout, io.Discard)

	timeoutResponse := performRequest(service, `{"content":"text"}`)
	if timeoutResponse.Code != http.StatusGatewayTimeout || decodeErrorResponse(t, timeoutResponse).Code != "upstream_timeout" {
		t.Fatalf("timeout response = %d %s", timeoutResponse.Code, timeoutResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/summarize", strings.NewReader(`{"content":"text"}`))
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	request = request.WithContext(ctx)
	canceledResponse := httptest.NewRecorder()
	service.ServeHTTP(canceledResponse, request)
	if canceledResponse.Code != http.StatusRequestTimeout || decodeErrorResponse(t, canceledResponse).Code != "request_canceled" {
		t.Fatalf("canceled response = %d %s", canceledResponse.Code, canceledResponse.Body.String())
	}
}

func TestEmptySummaryIsBadGateway(t *testing.T) {
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) { return "  ", nil }}
	service := testService(upstream, time.Second, io.Discard)
	response := performRequest(service, `{"content":"text"}`)
	if response.Code != http.StatusBadGateway || decodeErrorResponse(t, response).Code != "invalid_upstream_response" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestErrorsAndDiagnosticsDoNotExposeContent(t *testing.T) {
	var logs bytes.Buffer
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) {
		return "", errors.New("upstream included " + testSecret)
	}}
	service := testService(upstream, time.Second, &logs)
	response := performRequest(service, `{"content":"`+testSecret+`","style":"title_summary"}`)
	combined := response.Body.String() + logs.String()
	if strings.Contains(combined, testSecret) {
		t.Fatalf("sensitive content appeared in output: %s", combined)
	}
	if !strings.Contains(logs.String(), `"style":"title_summary"`) || !strings.Contains(logs.String(), `"outcome":"upstream_unavailable"`) {
		t.Fatalf("missing safe diagnostics: %s", logs.String())
	}
}

func TestOpenAIClientOptionsDisableAutomaticRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(testHTTPErrorBody))
	}))
	defer server.Close()

	options := append(openAIClientOptions("test-key"), option.WithBaseURL(server.URL+"/"), option.WithHTTPClient(server.Client()))
	client := openai.NewClient(options...)
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model:    "gpt-oss-120b",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("text")},
	})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if requests.Load() != 1 {
		t.Fatalf("request count = %d, want 1", requests.Load())
	}
}
