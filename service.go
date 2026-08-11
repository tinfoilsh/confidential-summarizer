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
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/sirupsen/logrus"

	"github.com/tinfoilsh/confidential-summarizer/config"
)

const (
	maxRequestBodyBytes    = 1 << 20
	minWordOverride        = 1
	maxWordOverride        = 1000
	minTokenOverride       = 1
	maxTokenOverride       = 32768
	defaultUpstreamTimeout = 30 * time.Second
)

var errEmptySummary = errors.New("upstream returned an empty summary")

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
	RequestID         string `json:"request_id"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

type serviceError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

type summaryService struct {
	upstream        summaryUpstream
	upstreamTimeout time.Duration
	logger          *logrus.Logger
}

func newSummaryService(upstream summaryUpstream, upstreamTimeout time.Duration, logger *logrus.Logger) *summaryService {
	return &summaryService{
		upstream:        upstream,
		upstreamTimeout: upstreamTimeout,
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

	summary, err := s.upstream.Summarize(callContext, input)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(callContext.Err(), context.DeadlineExceeded) {
		return "", context.DeadlineExceeded
	}
	if err == nil && strings.TrimSpace(summary) == "" {
		return "", errEmptySummary
	}
	return summary, err
}

func (s *summaryService) classifyError(err error) serviceError {
	if errors.Is(err, context.Canceled) {
		return serviceError{Status: http.StatusRequestTimeout, Code: "request_canceled", Message: "request was canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return serviceError{Status: http.StatusGatewayTimeout, Code: "upstream_timeout", Message: "upstream service timed out"}
	}
	if errors.Is(err, errEmptySummary) || isMalformedResponseError(err) {
		return serviceError{Status: http.StatusBadGateway, Code: "invalid_upstream_response", Message: "upstream service returned an invalid response"}
	}

	if statusCode, response, ok := upstreamErrorStatus(err); ok {
		retryAfter := retryAfterFromResponse(response)
		if statusCode == http.StatusTooManyRequests {
			return serviceError{Status: http.StatusTooManyRequests, Code: "upstream_rate_limited", Message: "upstream service is rate limited", RetryAfter: retryAfter}
		}
		if statusCode >= http.StatusInternalServerError || statusCode == http.StatusRequestTimeout || statusCode == http.StatusConflict {
			return serviceError{Status: http.StatusServiceUnavailable, Code: "upstream_unavailable", Message: "upstream service is temporarily unavailable", RetryAfter: retryAfter}
		}
		return serviceError{Status: http.StatusBadGateway, Code: "upstream_error", Message: "upstream service rejected the request", RetryAfter: retryAfter}
	}

	return serviceError{Status: http.StatusServiceUnavailable, Code: "upstream_unavailable", Message: "upstream service is temporarily unavailable"}
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
		return 0
	}
	value := response.Header.Get("Retry-After")
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		if delay := time.Until(retryAt); delay > 0 {
			return delay
		}
	}
	return 0
}

func (s *summaryService) writeError(w http.ResponseWriter, requestID string, responseError serviceError) {
	response := errorResponse{
		Error:     responseError.Message,
		Code:      responseError.Code,
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
