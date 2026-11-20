package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func newResponsesStreamAdapter(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent], systemService *biz.SystemService) (streams.Stream[*httpclient.StreamEvent], StreamErrorEncoder) {
	var nextSequence atomic.Int64
	source := stream
	stream = streams.MapErr(stream, func(event *httpclient.StreamEvent) (*httpclient.StreamEvent, error) {
		if seq := gjson.GetBytes(event.Data, "sequence_number"); seq.Type == gjson.Number && seq.Int() >= nextSequence.Load() {
			nextSequence.Store(seq.Int() + 1)
		}
		return applyResponsesEventErrorPolicy(ctx, event, systemService)
	})
	encodeErr := func(ctx context.Context, err error) (*httpclient.StreamEvent, error) {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			err = applyUpstreamErrorPolicy(ctx, pipeline.WrapUpstreamError(orchestrator.ClassifyUpstreamTransportError(err)), systemService)
		}
		return encodeResponsesStreamError(ctx, err, int(nextSequence.Load()))
	}
	return preserveStreamDelivery(stream, source), encodeErr
}

// encodeResponsesStreamError builds a Responses protocol error event for an error whose
// policy decision is already final. Callers that must not redact a gateway-local error
// (for example a failure before the upstream stream opened) pass the raw error here
// instead of the adapter's encoder, which always marks errors as upstream.
func encodeResponsesStreamError(ctx context.Context, err error, sequence int) (*httpclient.StreamEvent, error) {
	code := "stream_error"
	message := orchestrator.ExtractErrorMessage(err)
	requestID := ""
	var responseErr *llm.ResponseError
	var httpErr *httpclient.Error
	switch {
	case errors.As(err, &responseErr):
		code = firstNonEmpty(responseErr.Detail.Code, code)
		message = responseErr.Detail.Message
		requestID = responseErr.Detail.RequestID
	case errors.As(err, &httpErr):
		code = firstNonEmpty(upstreamErrorCodeFromHTTP(httpErr), code)
		requestID = upstreamRequestIDFromHTTP(httpErr)
	}
	data, marshalErr := json.Marshal(struct {
		responses.StreamEvent

		Param     *string `json:"param"`
		RequestID string  `json:"request_id,omitempty"`
	}{StreamEvent: responses.StreamEvent{
		Type: responses.StreamEventTypeError, SequenceNumber: sequence, Code: code, Message: message,
	}, RequestID: streamErrorRequestID(ctx, requestID)})
	if marshalErr != nil {
		return nil, marshalErr
	}
	return &httpclient.StreamEvent{Type: "error", Data: data}, nil
}

func applyResponsesEventErrorPolicy(ctx context.Context, event *httpclient.StreamEvent, systemService *biz.SystemService) (*httpclient.StreamEvent, error) {
	if systemService == nil {
		return event, nil
	}
	messagePath := responsesEventMessagePath(event)
	if messagePath == "" {
		return event, nil
	}
	policy := systemService.RetryPolicyOrDefault(ctx).UpstreamErrorPolicy
	if policy.Mode == "" || policy.Mode == biz.UpstreamErrorModePassthrough {
		return event, nil
	}
	if !gjson.GetBytes(event.Data, messagePath).Exists() {
		return event, nil
	}
	prefix := strings.TrimSuffix(messagePath, "message")
	responseErr := &llm.ResponseError{StatusCode: http.StatusBadGateway, Detail: llm.ErrorDetail{
		Message: gjson.GetBytes(event.Data, messagePath).String(),
		Code:    gjson.GetBytes(event.Data, prefix+"code").String(),
		Type:    gjson.GetBytes(event.Data, prefix+"type").String(),
	}}
	err := applyUpstreamErrorPolicy(ctx, pipeline.WrapUpstreamError(responseErr), systemService)
	data, rewriteErr := sjson.SetBytes(event.Data, messagePath, orchestrator.ExtractErrorMessage(err))
	if rewriteErr != nil {
		return nil, rewriteErr
	}
	copyEvent := *event
	copyEvent.Data = data
	return &copyEvent, nil
}

// responsesEventMessagePath returns the JSON path of the policy-controlled message on an
// error event, or "" for every other event type.
func responsesEventMessagePath(event *httpclient.StreamEvent) string {
	switch firstNonEmpty(event.Type, gjson.GetBytes(event.Data, "type").String()) {
	case "response.failed":
		return "response.error.message"
	case "error":
		if gjson.GetBytes(event.Data, "error.message").Exists() {
			return "error.message"
		}

		return "message"
	default:
		return ""
	}
}
