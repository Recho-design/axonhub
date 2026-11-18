package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestInboundTransformer_CleanEOFTextWithUsageSynthesizesFinishAndDone(t *testing.T) {
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		openAIInboundTextChunk("hello"),
		openAIInboundUsageChunk(),
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.Len(t, events, 4)

	var finish Response
	require.NoError(t, json.Unmarshal(events[2].Data, &finish))
	require.Equal(t, "stop", lo.FromPtr(finish.Choices[0].FinishReason))
	require.Equal(t, "[DONE]", string(events[3].Data))
}

func TestInboundTransformer_CleanEOFReasoningWithUsageSynthesizesFinishAndDone(t *testing.T) {
	reasoning := "thinking"
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		{
			ID:      "chatcmpl-reasoning",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{ReasoningContent: &reasoning}}},
		},
		openAIInboundUsageChunk(),
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.Equal(t, "stop", inboundFinishReason(t, events[2]))
	require.Equal(t, "[DONE]", string(events[3].Data))
}

func TestInboundTransformer_CleanEOFValidToolCallSynthesizesToolFinishAndDone(t *testing.T) {
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		{
			ID: "chatcmpl-tool",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
				Index:    0,
				Type:     "function",
				Function: llm.FunctionCall{Name: "lookup", Arguments: `{"q":"ping"}`},
			}}}}},
		},
		openAIInboundUsageChunk(),
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.Equal(t, "tool_calls", inboundFinishReason(t, events[2]))
	require.Equal(t, "[DONE]", string(events[3].Data))
}

func TestInboundTransformer_CleanEOFMalformedToolArgumentsDoesNotSucceed(t *testing.T) {
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		{
			ID: "chatcmpl-tool-invalid",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
				Index:    0,
				Type:     "function",
				Function: llm.FunctionCall{Name: "lookup", Arguments: `{"q":"unterminated`},
			}}}}},
		},
		openAIInboundUsageChunk(),
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.Len(t, events, 2)
	for _, event := range events {
		require.NotEqual(t, "[DONE]", string(event.Data))
	}
}

func TestInboundTransformer_UpstreamErrorDoesNotSynthesizeFinish(t *testing.T) {
	upstreamErr := errors.New("provider disconnected")
	source := &openAIInboundErrorStream{
		events: []*llm.Response{openAIInboundTextChunk("partial"), openAIInboundUsageChunk()},
		err:    upstreamErr,
	}
	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.ErrorIs(t, stream.Err(), upstreamErr)
	require.Len(t, events, 2)
	for _, event := range events {
		require.NotEqual(t, "[DONE]", string(event.Data))
	}
}

func openAIInboundTextChunk(content string) *llm.Response {
	return &llm.Response{
		ID: "chatcmpl-text",
		Choices: []llm.Choice{{
			Index: 0,
			Delta: &llm.Message{Content: llm.MessageContent{Content: &content}},
		}},
	}
}

func openAIInboundUsageChunk() *llm.Response {
	return &llm.Response{
		ID:    "chatcmpl-usage",
		Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

func collectOpenAIInboundEvents(t *testing.T, stream streams.Stream[*httpclient.StreamEvent]) []*httpclient.StreamEvent {
	t.Helper()
	var events []*httpclient.StreamEvent
	for stream.Next() {
		events = append(events, stream.Current())
	}
	return events
}

func inboundFinishReason(t *testing.T, event *httpclient.StreamEvent) string {
	t.Helper()
	var response Response
	require.NoError(t, json.Unmarshal(event.Data, &response))
	return lo.FromPtr(response.Choices[0].FinishReason)
}

// TestInboundTransformer_ParallelToolCallsFinishReasonOrdering guards the OpenAI
// SSE contract for parallel tool calls (e.g. Gemini chat upstreams that emit all
// functionCall parts in one chunk): every delta-carrying frame must have a null
// finish_reason, exactly one standalone finish frame (empty delta) may follow
// the last delta, and nothing but usage/[DONE] may come after it.
func TestInboundTransformer_ParallelToolCallsFinishReasonOrdering(t *testing.T) {
	mk := func(finish *string, calls ...int) *llm.Response {
		tools := make([]llm.ToolCall, 0, len(calls))
		for _, i := range calls {
			tools = append(tools, llm.ToolCall{
				Index: i,
				ID:    fmt.Sprintf("call_%d", i),
				Type:  "function",
				Function: llm.FunctionCall{
					Name:      "read",
					Arguments: fmt.Sprintf(`{"path":"f%d.txt"}`, i),
				},
			})
		}
		return &llm.Response{
			ID:     "chatcmpl-parallel",
			Object: "chat.completion.chunk",
			Model:  "gemini-4-carbon",
			Choices: []llm.Choice{{
				Index:        0,
				Delta:        &llm.Message{Role: "assistant", ToolCalls: tools},
				FinishReason: finish,
			}},
		}
	}

	// Reproduces the captured bug: upstream sends 3 parallel tool calls, with
	// finish_reason "tool_calls" glued onto the chunk that carries deltas, and
	// again on a later chunk after it.
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		mk(nil, 0),
		mk(nil, 0),
		mk(lo.ToPtr("tool_calls"), 1),
		mk(lo.ToPtr("tool_calls"), 2),
		{
			ID: "chatcmpl-parallel", Object: "chat.completion.chunk", Model: "gemini-4-carbon",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{}, FinishReason: lo.ToPtr("tool_calls")}},
		},
		{ID: "chatcmpl-parallel", Object: "chat.completion.chunk", Model: "gemini-4-carbon",
			Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{Object: "[DONE]"},
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.NotEmpty(t, events)
	require.Equal(t, "[DONE]", string(events[len(events)-1].Data))
	// data frames before [DONE]
	frames := events[:len(events)-1]
	require.NotEmpty(t, frames)

	finishAt := -1
	var toolCallIndexes []int
	for i, frame := range frames {
		var resp Response
		require.NoError(t, json.Unmarshal(frame.Data, &resp), "frame %d", i)

		for _, choice := range resp.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				// Finish frame must be unique and must not carry any delta payload.
				require.Equal(t, -1, finishAt, "finish_reason emitted more than once, at frames %d and %d", finishAt, i)
				require.NotNil(t, choice.Delta)
				require.Empty(t, choice.Delta.ToolCalls)
				require.Empty(t, choice.Delta.Content.MultipleContent)
				require.Nil(t, choice.Delta.Content.Content)
				finishAt = i
				continue
			}
			// Delta frames must precede the finish frame.
			require.Equal(t, -1, finishAt, "delta content after finish_reason at frame %d", i)
			if choice.Delta != nil {
				for _, tc := range choice.Delta.ToolCalls {
					toolCallIndexes = append(toolCallIndexes, tc.Index)
				}
			}
		}
	}

	// All three parallel tool calls (index 0,1,2) were delivered before finish.
	require.Equal(t, []int{0, 0, 1, 2}, toolCallIndexes)
	require.GreaterOrEqual(t, finishAt, 0, "a finish frame must be emitted")
}

// TestInboundTransformer_ToolCallFinishSeparation_Scenarios covers the single
// tool call, text+tools mix, MAX_TOKENS/SAFETY truncation, and delta-free
// finish chunks, ensuring finish_reason is emitted exactly once at the end.
func TestInboundTransformer_ToolCallFinishSeparation_Scenarios(t *testing.T) {
	newChunk := func(object string, delta *llm.Message, finish *string, usage *llm.Usage) *llm.Response {
		return &llm.Response{
			ID: "chatcmpl-x", Object: object, Model: "m", Created: 1,
			Choices: []llm.Choice{{Index: 0, Delta: delta, FinishReason: finish}},
			Usage:   usage,
		}
	}

	t.Run("single tool call with finish on same chunk", func(t *testing.T) {
		stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
			newChunk("chat.completion.chunk", &llm.Message{ToolCalls: []llm.ToolCall{{
				Index: 0, ID: "c1", Type: "function",
				Function: llm.FunctionCall{Name: "read", Arguments: `{"a":1}`},
			}}}, lo.ToPtr("tool_calls"), nil),
			{Object: "[DONE]"},
		}))
		require.NoError(t, err)
		events := collectOpenAIInboundEvents(t, stream)
		require.NoError(t, stream.Err())
		require.Len(t, events, 3)
		var first, second Response
		require.NoError(t, json.Unmarshal(events[0].Data, &first))
		require.NoError(t, json.Unmarshal(events[1].Data, &second))
		require.Nil(t, first.Choices[0].FinishReason)
		require.Len(t, first.Choices[0].Delta.ToolCalls, 1)
		require.Equal(t, "tool_calls", lo.FromPtr(second.Choices[0].FinishReason))
		require.Empty(t, second.Choices[0].Delta.ToolCalls)
		require.Equal(t, "[DONE]", string(events[2].Data))
	})

	t.Run("text plus tools with finish on same chunk", func(t *testing.T) {
		text := "checking"
		stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
			newChunk("chat.completion.chunk", &llm.Message{Content: llm.MessageContent{Content: &text}}, nil, nil),
			newChunk("chat.completion.chunk", &llm.Message{ToolCalls: []llm.ToolCall{{
				Index: 0, ID: "c1", Type: "function",
				Function: llm.FunctionCall{Name: "read", Arguments: `{"a":1}`},
			}}}, lo.ToPtr("tool_calls"), nil),
			{Object: "[DONE]"},
		}))
		require.NoError(t, err)
		events := collectOpenAIInboundEvents(t, stream)
		require.NoError(t, stream.Err())
		require.Len(t, events, 4)
		var finish Response
		require.NoError(t, json.Unmarshal(events[2].Data, &finish))
		require.Equal(t, "tool_calls", lo.FromPtr(finish.Choices[0].FinishReason))
		require.Empty(t, finish.Choices[0].Delta.ToolCalls)
	})

	t.Run("truncated stream keeps mapped finish reason", func(t *testing.T) {
		// MAX_TOKENS while streaming text: finish must be preserved and emitted last.
		text := "partial output"
		stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
			newChunk("chat.completion.chunk", &llm.Message{Content: llm.MessageContent{Content: &text}}, lo.ToPtr("length"), nil),
			{Object: "[DONE]"},
		}))
		require.NoError(t, err)
		events := collectOpenAIInboundEvents(t, stream)
		require.NoError(t, stream.Err())
		require.Len(t, events, 3)
		var first Response
		require.NoError(t, json.Unmarshal(events[0].Data, &first))
		require.Nil(t, first.Choices[0].FinishReason)
		var finish Response
		require.NoError(t, json.Unmarshal(events[1].Data, &finish))
		require.Equal(t, "length", lo.FromPtr(finish.Choices[0].FinishReason))
		require.Nil(t, finish.Choices[0].Delta.Content.Content)
		require.Equal(t, "[DONE]", string(events[2].Data))
	})

	t.Run("delta-free finish chunk is deferred to stream end", func(t *testing.T) {
		text := "hello"
		stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
			newChunk("chat.completion.chunk", &llm.Message{Content: llm.MessageContent{Content: &text}}, nil, nil),
			newChunk("chat.completion.chunk", &llm.Message{}, lo.ToPtr("stop"), &llm.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}),
			{Object: "[DONE]"},
		}))
		require.NoError(t, err)
		events := collectOpenAIInboundEvents(t, stream)
		require.NoError(t, stream.Err())
		// text delta, deferred finish frame, standalone usage frame, [DONE].
		require.Len(t, events, 4)
		var finish Response
		require.NoError(t, json.Unmarshal(events[1].Data, &finish))
		require.Equal(t, "stop", lo.FromPtr(finish.Choices[0].FinishReason))
		require.Empty(t, finish.Choices[0].Delta.ToolCalls)
		// Usage follows the finish frame as a choices-free chunk, per
		// stream_options.include_usage semantics.
		var usage Response
		require.NoError(t, json.Unmarshal(events[2].Data, &usage))
		require.Nil(t, finish.Usage)
		require.NotNil(t, usage.Usage)
		require.Empty(t, usage.Choices)
		require.Empty(t, finish.Choices[0].Delta.ToolCalls)
	})
}

// TestInboundTransformer_TerminalChunkThenMoreDeltasNeverLeaksFinish guards the
// strongest adversarial shape: an upstream that sends a delta-free terminal
// chunk early and then keeps streaming deltas for the same choice. The finish
// must still land after every delta, exactly once.
func TestInboundTransformer_TerminalChunkThenMoreDeltasNeverLeaksFinish(t *testing.T) {
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		{
			ID: "chatcmpl-leak", Object: "chat.completion.chunk",
			Choices: []llm.Choice{{
				Index:        0,
				Delta:        &llm.Message{ToolCalls: []llm.ToolCall{{Index: 0, ID: "c0", Type: "function", Function: llm.FunctionCall{Name: "read", Arguments: `{"a":1}`}}}},
				FinishReason: lo.ToPtr("tool_calls"),
			}},
		},
		// Bogus early terminal chunk, followed by MORE deltas for the same choice.
		{
			ID: "chatcmpl-leak", Object: "chat.completion.chunk",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{}, FinishReason: lo.ToPtr("tool_calls")}},
		},
		{
			ID: "chatcmpl-leak", Object: "chat.completion.chunk",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
				Index: 1, ID: "c1", Type: "function",
				Function: llm.FunctionCall{Name: "read", Arguments: `{"b":2}`},
			}}}}},
		},
		{Object: "[DONE]"},
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.Equal(t, "[DONE]", string(events[len(events)-1].Data))
	frames := events[:len(events)-1]

	finishAt := -1
	for i, frame := range frames {
		var resp Response
		require.NoError(t, json.Unmarshal(frame.Data, &resp))
		for _, choice := range resp.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				require.Equal(t, -1, finishAt, "duplicate finish frame at %d", i)
				require.Empty(t, choice.Delta.ToolCalls)
				finishAt = i
				continue
			}
			require.Equal(t, -1, finishAt, "delta content after finish at frame %d", i)
		}
	}
	require.GreaterOrEqual(t, finishAt, 0)
}

// TestInboundTransformer_SpecificFinishReasonNeverDowngraded guards the rank
// merge: a truncation reason stripped from an earlier delta chunk must not be
// overridden by a later generic delta-free terminal chunk.
func TestInboundTransformer_SpecificFinishReasonNeverDowngraded(t *testing.T) {
	text := "partial"
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		{
			ID: "chatcmpl-rank", Object: "chat.completion.chunk",
			Choices: []llm.Choice{{
				Index:        0,
				Delta:        &llm.Message{Content: llm.MessageContent{Content: &text}},
				FinishReason: lo.ToPtr("length"),
			}},
		},
		{
			ID: "chatcmpl-rank", Object: "chat.completion.chunk",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{}, FinishReason: lo.ToPtr("stop")}},
		},
		{Object: "[DONE]"},
	}))
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.NoError(t, stream.Err())
	require.Len(t, events, 3)

	var finish Response
	require.NoError(t, json.Unmarshal(events[1].Data, &finish))
	require.Equal(t, "length", lo.FromPtr(finish.Choices[0].FinishReason), "truncation reason must win over later generic stop")
	require.Equal(t, "[DONE]", string(events[2].Data))
}

// TestInboundTransformer_MidStreamErrorDropsFinishWithIncompleteTools ensures a
// deferred finish is NOT replayed when the choice's aggregated tool call output
// is incomplete — clients must not mistake a truncated turn for an executable one.
func TestInboundTransformer_MidStreamErrorDropsFinishWithIncompleteToolArguments(t *testing.T) {
	streamErr := errors.New("boom")
	source := &openAIInboundErrorStream{
		events: []*llm.Response{
			{
				ID: "chatcmpl-trunc", Object: "chat.completion.chunk",
				Choices: []llm.Choice{{
					Index: 0,
					Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
						Index: 0, ID: "c1", Type: "function",
						Function: llm.FunctionCall{Name: "read", Arguments: `{"a":`},
					}}},
					FinishReason: lo.ToPtr("tool_calls"),
				}},
			},
		},
		err: streamErr,
	}

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.ErrorIs(t, stream.Err(), streamErr)
	// Only the delta frame; the truncated choice gets no finish frame.
	require.Len(t, events, 1)
	var delta Response
	require.NoError(t, json.Unmarshal(events[0].Data, &delta))
	require.Nil(t, delta.Choices[0].FinishReason)
}

// TestInboundTransformer_MidStreamErrorPreservesUpstreamFinish ensures a real
// finish reason that arrived glued to a delta chunk is still delivered when the
// stream later fails — clients must see the finish frame before the abort, but
// never any delta content after finish_reason.
func TestInboundTransformer_MidStreamErrorPreservesUpstreamFinish(t *testing.T) {
	streamErr := errors.New("boom")
	source := &openAIInboundErrorStream{
		events: []*llm.Response{
			{
				ID: "chatcmpl-err", Object: "chat.completion.chunk",
				Choices: []llm.Choice{{
					Index: 0,
					Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
						Index: 0, ID: "c1", Type: "function",
						Function: llm.FunctionCall{Name: "read", Arguments: `{"a":1}`},
					}}},
					FinishReason: lo.ToPtr("tool_calls"),
				}},
			},
		},
		err: streamErr,
	}

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)

	events := collectOpenAIInboundEvents(t, stream)
	require.ErrorIs(t, stream.Err(), streamErr)
	// Delta frame first, then the deferred real finish frame; no [DONE].
	require.Len(t, events, 2)

	var delta Response
	require.NoError(t, json.Unmarshal(events[0].Data, &delta))
	require.Nil(t, delta.Choices[0].FinishReason)
	require.Len(t, delta.Choices[0].Delta.ToolCalls, 1)

	var finish Response
	require.NoError(t, json.Unmarshal(events[1].Data, &finish))
	require.Equal(t, "tool_calls", lo.FromPtr(finish.Choices[0].FinishReason))
	require.Empty(t, finish.Choices[0].Delta.ToolCalls)
}

type openAIInboundErrorStream struct {
	events []*llm.Response
	index  int
	err    error
}

func (s *openAIInboundErrorStream) Next() bool {
	return s.index < len(s.events)
}

func (s *openAIInboundErrorStream) Current() *llm.Response {
	response := s.events[s.index]
	s.index++
	return response
}

func (s *openAIInboundErrorStream) Err() error {
	if s.index >= len(s.events) {
		return s.err
	}
	return nil
}

func (s *openAIInboundErrorStream) Close() error { return nil }
