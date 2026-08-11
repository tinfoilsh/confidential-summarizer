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
	"sync"
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
	testCircuitDelay  = 20 * time.Millisecond
	testRequestWait   = time.Second
	testTimeout       = 10 * time.Millisecond
	testHTTPErrorBody = `{"error":{"message":"unavailable","type":"server_error"}}`
)

type fakeUpstream struct {
	summarize func(context.Context, summaryInput) (string, error)
}

func (f fakeUpstream) Summarize(ctx context.Context, input summaryInput) (string, error) {
	return f.summarize(ctx, input)
}

func testService(upstream summaryUpstream, maxConcurrency int, resilience resilienceConfig, output io.Writer) *summaryService {
	logger := logrus.New()
	logger.SetOutput(output)
	logger.SetFormatter(&logrus.JSONFormatter{})
	return newSummaryService(upstream, maxConcurrency, resilience, logger)
}

func testResilience() resilienceConfig {
	return resilienceConfig{
		UpstreamTimeout:     time.Second,
		BreakerFailureLimit: 2,
		BreakerBaseCooldown: testCircuitDelay,
		BreakerMaxCooldown:  4 * testCircuitDelay,
	}
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

func TestConfiguredConcurrency(t *testing.T) {
	if value, err := configuredConcurrency(""); err != nil || value != defaultMaxConcurrency {
		t.Fatalf("default concurrency = %d, err = %v", value, err)
	}
	if value, err := configuredConcurrency("32"); err != nil || value != 32 {
		t.Fatalf("configured concurrency = %d, err = %v", value, err)
	}
	for _, value := range []string{"0", "1025", "invalid"} {
		if _, err := configuredConcurrency(value); err == nil {
			t.Fatalf("expected concurrency %q to be rejected", value)
		}
	}
}

func TestExponentialCooldownIsCapped(t *testing.T) {
	base := 5 * time.Second
	maximum := 20 * time.Second
	for opening, expected := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 20 * time.Second} {
		if actual := exponentialCooldown(base, maximum, uint32(opening+1)); actual != expected {
			t.Fatalf("opening %d cooldown = %s, want %s", opening+1, actual, expected)
		}
	}
}

func TestRequestValidation(t *testing.T) {
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) {
		return testSummary, nil
	}}
	service := testService(upstream, defaultMaxConcurrency, testResilience(), io.Discard)

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
			if body.Error == "" || body.Message != body.Error {
				t.Fatalf("error envelope is not backward compatible: %+v", body)
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
	service := testService(upstream, defaultMaxConcurrency, testResilience(), io.Discard)
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
		{name: "transient", err: &openai.Error{StatusCode: http.StatusBadGateway}, status: http.StatusServiceUnavailable, code: "upstream_unavailable", retryAfter: "1"},
		{name: "upstream rejection", err: &openai.Error{StatusCode: http.StatusBadRequest}, status: http.StatusBadGateway, code: "upstream_error"},
		{name: "malformed response", err: &json.SyntaxError{}, status: http.StatusBadGateway, code: "invalid_upstream_response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) { return "", test.err }}
			service := testService(upstream, defaultMaxConcurrency, testResilience(), io.Discard)
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
			service := testService(newOpenAIUpstream(&client, "gpt-oss-120b"), defaultMaxConcurrency, testResilience(), io.Discard)
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
	resilience := testResilience()
	resilience.UpstreamTimeout = testTimeout
	service := testService(upstream, defaultMaxConcurrency, resilience, io.Discard)

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

func TestConcurrencyLimitRejectsImmediately(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequest)
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) {
		close(started)
		<-release
		return testSummary, nil
	}}
	service := testService(upstream, 1, testResilience(), io.Discard)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- performRequest(service, `{"content":"first"}`) }()

	select {
	case <-started:
	case <-time.After(testRequestWait):
		t.Fatal("first request did not reach upstream")
	}
	second := performRequest(service, `{"content":"second"}`)
	if second.Code != http.StatusServiceUnavailable || decodeErrorResponse(t, second).Code != "overloaded" {
		t.Fatalf("second response = %d %s", second.Code, second.Body.String())
	}
	if second.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", second.Header().Get("Retry-After"))
	}
	releaseRequest()
	if first := <-firstDone; first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
}

func TestCircuitOpensAndRecoversWithSingleProbe(t *testing.T) {
	var calls atomic.Int32
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(releaseProbe) }) }
	t.Cleanup(releaseRequest)
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) {
		call := calls.Add(1)
		if call <= 2 {
			return "", errors.New("temporary failure")
		}
		if call == 3 {
			close(probeStarted)
			<-releaseProbe
		}
		return testSummary, nil
	}}
	service := testService(upstream, defaultMaxConcurrency, testResilience(), io.Discard)

	for range 2 {
		if response := performRequest(service, `{"content":"text"}`); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("failure status = %d", response.Code)
		}
	}
	openResponse := performRequest(service, `{"content":"text"}`)
	if decodeErrorResponse(t, openResponse).Code != "circuit_open" || calls.Load() != 2 {
		t.Fatalf("circuit did not reject request, calls=%d body=%s", calls.Load(), openResponse.Body.String())
	}

	time.Sleep(testCircuitDelay + testTimeout)
	probeDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { probeDone <- performRequest(service, `{"content":"probe"}`) }()
	select {
	case <-probeStarted:
	case <-time.After(testRequestWait):
		t.Fatal("half-open probe did not start")
	}
	concurrentProbe := performRequest(service, `{"content":"second probe"}`)
	if decodeErrorResponse(t, concurrentProbe).Code != "circuit_open" || calls.Load() != 3 {
		t.Fatalf("concurrent probe was not rejected, calls=%d body=%s", calls.Load(), concurrentProbe.Body.String())
	}
	releaseRequest()
	if response := <-probeDone; response.Code != http.StatusOK {
		t.Fatalf("probe status = %d", response.Code)
	}
	if response := performRequest(service, `{"content":"recovered"}`); response.Code != http.StatusOK || calls.Load() != 4 {
		t.Fatalf("recovery status = %d calls=%d", response.Code, calls.Load())
	}
}

func TestEmptySummaryIsBadGateway(t *testing.T) {
	upstream := fakeUpstream{summarize: func(context.Context, summaryInput) (string, error) { return "  ", nil }}
	service := testService(upstream, defaultMaxConcurrency, testResilience(), io.Discard)
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
	service := testService(upstream, defaultMaxConcurrency, testResilience(), &logs)
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
