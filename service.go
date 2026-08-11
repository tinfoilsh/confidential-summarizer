package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-summarizer/config"
)

const (
	defaultMaxConcurrency  = 16
	minMaxConcurrency      = 1
	maxMaxConcurrency      = 1024
	maxRequestBodyBytes    = 1 << 20
	minWordOverride        = 1
	maxWordOverride        = 1000
	minTokenOverride       = 1
	maxTokenOverride       = 32768
	defaultRetryAfter      = time.Second
	defaultUpstreamTimeout = 30 * time.Second
	breakerFailureLimit    = 5
	breakerBaseCooldown    = 5 * time.Second
	breakerMaxCooldown     = time.Minute
)

var (
	errEmptySummary    = errors.New("upstream returned an empty summary")
	errRequestCanceled = errors.New("request canceled")
	errUpstreamTimeout = errors.New("upstream timeout")
)

type summarizeRequest struct {
	Content   string `json:"content"`
	Style     string `json:"style,omitempty"`
	MinWords  *int   `json:"min_words,omitempty"`
	MaxWords  *int   `json:"max_words,omitempty"`
	MaxTokens *int   `json:"max_tokens,omitempty"`
}

type summarizeResponse struct {
	Summary string `json:"summary"`
}

type errorResponse struct {
	Error             string `json:"error"`
	Code              string `json:"code"`
	Message           string `json:"message"`
	RequestID         string `json:"request_id"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

type serviceError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

type resilienceConfig struct {
	UpstreamTimeout     time.Duration
	BreakerFailureLimit uint
	BreakerBaseCooldown time.Duration
	BreakerMaxCooldown  time.Duration
}

type summaryService struct {
	upstream        summaryUpstream
	concurrency     chan struct{}
	upstreamTimeout time.Duration
	breaker         circuitbreaker.CircuitBreaker[string]
	logger          *logrus.Logger
}

func defaultResilienceConfig() resilienceConfig {
	return resilienceConfig{
		UpstreamTimeout:     defaultUpstreamTimeout,
		BreakerFailureLimit: breakerFailureLimit,
		BreakerBaseCooldown: breakerBaseCooldown,
		BreakerMaxCooldown:  breakerMaxCooldown,
	}
}

func newSummaryService(upstream summaryUpstream, maxConcurrency int, resilience resilienceConfig, logger *logrus.Logger) *summaryService {
	var openings atomic.Uint32
	breaker := circuitbreaker.NewBuilder[string]().
		HandleIf(func(_ string, err error) bool { return isCircuitFailure(err) }).
		WithFailureThreshold(resilience.BreakerFailureLimit).
		WithSuccessThreshold(1).
		WithDelayFunc(func(failsafe.ExecutionAttempt[string]) time.Duration {
			opening := openings.Add(1)
			return exponentialCooldown(resilience.BreakerBaseCooldown, resilience.BreakerMaxCooldown, opening)
		}).
		OnClose(func(circuitbreaker.StateChangedEvent) { openings.Store(0) }).
		Build()

	return &summaryService{
		upstream:        upstream,
		concurrency:     make(chan struct{}, maxConcurrency),
		upstreamTimeout: resilience.UpstreamTimeout,
		breaker:         breaker,
		logger:          logger,
	}
}

func (s *summaryService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", requestID)
	started := time.Now()
	styleName := "invalid"
	outcome := "error"
	defer func() {
		s.logger.WithFields(logrus.Fields{
			"latency_ms": time.Since(started).Milliseconds(),
			"outcome":    outcome,
			"request_id": requestID,
			"style":      styleName,
		}).Info("summarize request completed")
	}()

	if r.Method != http.MethodPost {
		outcome = "method_not_allowed"
		s.writeError(w, requestID, serviceError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"})
		return
	}

	request, input, validationErr := decodeAndValidateRequest(w, r)
	if validationErr != nil {
		outcome = validationErr.Code
		s.writeError(w, requestID, *validationErr)
		return
	}
	styleName = request.Style

	select {
	case s.concurrency <- struct{}{}:
		defer func() { <-s.concurrency }()
	default:
		outcome = "overloaded"
		s.writeError(w, requestID, serviceError{Status: http.StatusServiceUnavailable, Code: "overloaded", Message: "service is at capacity", RetryAfter: defaultRetryAfter})
		return
	}

	summary, err := s.summarize(r.Context(), input)
	if err != nil {
		responseError := s.classifyError(err)
		outcome = responseError.Code
		s.writeError(w, requestID, responseError)
		return
	}

	outcome = "success"
	if err := json.NewEncoder(w).Encode(summarizeResponse{Summary: summary}); err != nil {
		outcome = "response_write_failed"
		s.logger.WithFields(logrus.Fields{"outcome": "response_write_failed", "request_id": requestID, "style": styleName}).Warn("failed to write summarize response")
	}
}

func decodeAndValidateRequest(w http.ResponseWriter, r *http.Request) (summarizeRequest, summaryInput, *serviceError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	var request summarizeRequest
	if err := decoder.Decode(&request); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return request, summaryInput{}, &serviceError{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Message: "request body is too large"}
		}
		return request, summaryInput{}, &serviceError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "request body must be valid JSON"}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return request, summaryInput{}, &serviceError{Status: http.StatusRequestEntityTooLarge, Code: "request_too_large", Message: "request body is too large"}
		}
		return request, summaryInput{}, &serviceError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "request body must contain one JSON object"}
	}
	if strings.TrimSpace(request.Content) == "" {
		return request, summaryInput{}, &serviceError{Status: http.StatusBadRequest, Code: "invalid_content", Message: "content is required"}
	}

	if request.Style == "" {
		request.Style = config.DefaultStyle
	}
	style, ok := config.Styles[request.Style]
	if !ok {
		return request, summaryInput{}, &serviceError{Status: http.StatusBadRequest, Code: "invalid_style", Message: "style is not supported"}
	}

	minWords := overrideValue(request.MinWords, style.MinWords)
	maxWords := overrideValue(request.MaxWords, style.MaxWords)
	maxTokens := overrideValue(request.MaxTokens, style.MaxTokens)
	if minWords < minWordOverride || minWords > maxWordOverride || maxWords < minWordOverride || maxWords > maxWordOverride || minWords > maxWords {
		return request, summaryInput{}, &serviceError{Status: http.StatusBadRequest, Code: "invalid_word_range", Message: "word limits must be between 1 and 1000 and min_words must not exceed max_words"}
	}
	if maxTokens < minTokenOverride || maxTokens > maxTokenOverride {
		return request, summaryInput{}, &serviceError{Status: http.StatusBadRequest, Code: "invalid_max_tokens", Message: "max_tokens must be between 1 and 32768"}
	}

	return request, summaryInput{Content: request.Content, Style: style, MinWords: minWords, MaxWords: maxWords, MaxTokens: maxTokens}, nil
}

func overrideValue(override *int, fallback int) int {
	if override != nil {
		return *override
	}
	return fallback
}

func (s *summaryService) summarize(ctx context.Context, input summaryInput) (string, error) {
	callContext, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()

	summary, err := failsafe.With(s.breaker).WithContext(callContext).Get(func() (string, error) {
		result, upstreamErr := s.upstream.Summarize(callContext, input)
		if upstreamErr == nil && strings.TrimSpace(result) == "" {
			return "", errEmptySummary
		}
		return result, upstreamErr
	})
	if ctx.Err() != nil {
		return "", errRequestCanceled
	}
	if errors.Is(callContext.Err(), context.DeadlineExceeded) {
		return "", errUpstreamTimeout
	}
	return summary, err
}

func (s *summaryService) classifyError(err error) serviceError {
	if errors.Is(err, circuitbreaker.ErrOpen) {
		return serviceError{Status: http.StatusServiceUnavailable, Code: "circuit_open", Message: "upstream service is temporarily unavailable", RetryAfter: nonzeroRetryAfter(s.breaker.RemainingDelay())}
	}
	if errors.Is(err, errRequestCanceled) || errors.Is(err, context.Canceled) {
		return serviceError{Status: http.StatusRequestTimeout, Code: "request_canceled", Message: "request was canceled"}
	}
	if errors.Is(err, errUpstreamTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return serviceError{Status: http.StatusGatewayTimeout, Code: "upstream_timeout", Message: "upstream service timed out", RetryAfter: defaultRetryAfter}
	}
	if errors.Is(err, errEmptySummary) || isMalformedResponseError(err) {
		return serviceError{Status: http.StatusBadGateway, Code: "invalid_upstream_response", Message: "upstream service returned an invalid response"}
	}

	if statusCode, response, ok := upstreamErrorStatus(err); ok {
		if statusCode == http.StatusTooManyRequests {
			return serviceError{Status: http.StatusTooManyRequests, Code: "upstream_rate_limited", Message: "upstream service is rate limited", RetryAfter: retryAfterFromResponse(response)}
		}
		if statusCode >= http.StatusInternalServerError || statusCode == http.StatusRequestTimeout || statusCode == http.StatusConflict {
			return serviceError{Status: http.StatusServiceUnavailable, Code: "upstream_unavailable", Message: "upstream service is temporarily unavailable", RetryAfter: defaultRetryAfter}
		}
		return serviceError{Status: http.StatusBadGateway, Code: "upstream_error", Message: "upstream service rejected the request"}
	}

	return serviceError{Status: http.StatusServiceUnavailable, Code: "upstream_unavailable", Message: "upstream service is temporarily unavailable", RetryAfter: defaultRetryAfter}
}

func isCircuitFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errRequestCanceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errEmptySummary) || isMalformedResponseError(err) {
		return true
	}

	if statusCode, _, ok := upstreamErrorStatus(err); ok {
		return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError || statusCode == http.StatusRequestTimeout || statusCode == http.StatusConflict
	}
	return true
}

func upstreamErrorStatus(err error) (int, *http.Response, bool) {
	var apiError *openai.Error
	if errors.As(err, &apiError) {
		return apiError.StatusCode, apiError.Response, true
	}

	var httpError *upstreamHTTPError
	if errors.As(err, &httpError) {
		return httpError.statusCode, httpError.response, true
	}
	return 0, nil, false
}

func isMalformedResponseError(err error) bool {
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	return errors.As(err, &syntaxError) || errors.As(err, &typeError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func retryAfterFromResponse(response *http.Response) time.Duration {
	if response == nil {
		return defaultRetryAfter
	}
	value := response.Header.Get("Retry-After")
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return nonzeroRetryAfter(time.Until(retryAt))
	}
	return defaultRetryAfter
}

func nonzeroRetryAfter(delay time.Duration) time.Duration {
	if delay < time.Second {
		return defaultRetryAfter
	}
	return delay
}

func exponentialCooldown(base, maximum time.Duration, opening uint32) time.Duration {
	shift := opening - 1
	if shift > 30 {
		return maximum
	}
	delay := base * time.Duration(uint64(1)<<shift)
	if delay <= 0 || delay > maximum {
		return maximum
	}
	return delay
}

func (s *summaryService) writeError(w http.ResponseWriter, requestID string, responseError serviceError) {
	response := errorResponse{
		Error:     responseError.Message,
		Code:      responseError.Code,
		Message:   responseError.Message,
		RequestID: requestID,
	}
	if responseError.RetryAfter > 0 {
		response.RetryAfterSeconds = int(math.Ceil(responseError.RetryAfter.Seconds()))
		w.Header().Set("Retry-After", strconv.Itoa(response.RetryAfterSeconds))
	}
	w.WriteHeader(responseError.Status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.logger.WithField("request_id", requestID).Warn("failed to write error response")
	}
}
