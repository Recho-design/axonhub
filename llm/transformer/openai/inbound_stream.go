package openai

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
)

type openAIInboundChoice struct {
	output   bool
	finished bool
	tools    map[int]*llm.ToolCall
}

type openAIInboundStream struct {
	source  streams.Stream[*llm.Response]
	ctx     context.Context
	current *llm.Response
	queue   []*llm.Response
	choices map[int]*openAIInboundChoice
	usage   bool
	done    bool
	ended   bool
	id      string
	model   string
	created int64

	// pendingFinish records finish reasons stripped from chunks, keyed by
	// choice index. They are re-emitted in a dedicated terminal frame after
	// every delta frame, so clients never see content after finish_reason.
	pendingFinish map[int]string
	// pendingUsage holds a usage payload received while a finish reason was
	// still deferred, so the canonical finish -> usage -> [DONE] ordering holds.
	pendingUsage *llm.Usage
}

func newOpenAIInboundStream(source streams.Stream[*llm.Response], ctx context.Context) streams.Stream[*llm.Response] {
	return &openAIInboundStream{
		source:        source,
		ctx:           ctx,
		choices:       make(map[int]*openAIInboundChoice),
		pendingFinish: make(map[int]string),
	}
}

func (s *openAIInboundStream) Next() bool {
	if len(s.queue) > 0 {
		s.current = s.queue[0]
		s.queue = s.queue[1:]
		return true
	}
	if s.ended {
		return false
	}
	for s.source.Next() {
		s.current = s.source.Current()
		if s.current == nil {
			continue
		}
		s.observe(s.current)
		if s.current.Object == "[DONE]" {
			// Terminal frames must precede the [DONE] marker.
			s.queue = append(s.queue, s.takeFinishFrames(acceptAllChoices)...)
			s.enqueueUsage()
			s.queue = append(s.queue, s.current)
			return s.Next()
		}
		if separated := s.separateFinishReason(s.current); separated != nil {
			s.current = separated
			return true
		}
	}
	s.ended = true
	if s.source.Err() != nil || s.ctx.Err() != nil {
		// A finish reason the upstream really sent (deferred above) is still
		// delivered before the stream aborts, matching passthrough behavior —
		// but only for choices whose aggregated output is actually complete
		// (e.g. well-formed tool call arguments), so clients never mistake a
		// truncated turn for an executable one.
		s.queue = append(s.queue, s.takeFinishFrames(s.verifyChoiceComplete)...)
		s.enqueueUsage()
		return s.Next()
	}
	s.queue = append(s.queue, s.takeFinishFrames(acceptAllChoices)...)
	synthesized := s.synthesizeFinish()
	s.queue = append(s.queue, synthesized...)
	s.enqueueUsage()
	if len(synthesized) > 0 {
		s.queue = append(s.queue, llm.DoneResponse)
	}
	return s.Next()
}

// enqueueUsage appends a standalone usage frame for a held usage payload.
func (s *openAIInboundStream) enqueueUsage() {
	if usage := s.takePendingUsage(); usage != nil {
		s.queue = append(s.queue, usage)
	}
}

func (s *openAIInboundStream) Current() *llm.Response { return s.current }
func (s *openAIInboundStream) Err() error             { return s.source.Err() }
func (s *openAIInboundStream) Close() error           { return s.source.Close() }

func (s *openAIInboundStream) observe(response *llm.Response) {
	if response.Object == "[DONE]" {
		s.done = true
		return
	}
	if response.ID != "" {
		s.id = response.ID
	}
	if response.Model != "" {
		s.model = response.Model
	}
	if response.Created != 0 {
		s.created = response.Created
	}
	if response.Usage != nil {
		s.usage = true
	}
	for _, choice := range response.Choices {
		state := s.choices[choice.Index]
		if state == nil {
			state = &openAIInboundChoice{tools: make(map[int]*llm.ToolCall)}
			s.choices[choice.Index] = state
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			state.finished = true
		}
		for _, message := range []*llm.Message{choice.Delta, choice.Message} {
			if message == nil {
				continue
			}
			if message.Content.Content != nil && *message.Content.Content != "" || len(message.Content.MultipleContent) > 0 ||
				message.ReasoningContent != nil && *message.ReasoningContent != "" || message.Reasoning != nil && *message.Reasoning != "" || message.Refusal != "" || message.Audio != nil {
				state.output = true
			}
			for _, tool := range message.ToolCalls {
				state.output = true
				if state.tools[tool.Index] == nil {
					state.tools[tool.Index] = &llm.ToolCall{}
				}
				acc := state.tools[tool.Index]
				if tool.Function.Name != "" {
					acc.Function.Name = tool.Function.Name
				}
				acc.Function.Arguments += tool.Function.Arguments
			}
		}
	}
}

// separateFinishReason keeps the OpenAI SSE contract: at most one non-empty
// finish_reason per choice, never on a chunk that still carries deltas, and
// nothing but usage/[DONE] after it. Every non-empty finish_reason — including
// upstream delta-free terminal chunks — is deferred into pendingFinish and
// replayed as a standalone terminal frame (empty delta) at stream end; the
// strongest reason wins when several arrive. Usage that arrives while a finish
// is deferred is held back so it is emitted after the terminal frame.
// It returns nil when the whole chunk was buffered.
func (s *openAIInboundStream) separateFinishReason(resp *llm.Response) *llm.Response {
	out := resp

	clone := func() {
		if out == resp {
			cp := new(llm.Response)
			*cp = *resp
			cp.Choices = make([]llm.Choice, len(resp.Choices))
			copy(cp.Choices, resp.Choices)
			out = cp
		}
	}

	for i := range resp.Choices {
		choice := &resp.Choices[i]
		if choice.FinishReason == nil || *choice.FinishReason == "" {
			continue
		}

		// Always defer: even a delta-free terminal chunk may be followed by more
		// deltas from the same choice (observed from Gemini parallel tool calls),
		// and a late generic "stop" must not override a stronger earlier reason
		// such as "length"/"content_filter".
		s.deferFinish(choice.Index, *choice.FinishReason)
		clone()
		out.Choices[i].FinishReason = nil
	}

	if resp.Usage != nil && len(s.pendingFinish) > 0 {
		// Keep usage for after the deferred terminal frame. Overwrite semantics:
		// providers that attach cumulative usage to every chunk keep the latest.
		s.pendingUsage = resp.Usage
		clone()
		out.Usage = nil
	}

	if out != resp && !responseCarriesPayload(out) {
		// The chunk carried nothing but deferred finish/usage; buffer it whole.
		return nil
	}

	if out != resp {
		return out
	}

	return resp
}

// finishReasonRank orders finish reasons by information value: specific
// truncation/filtering causes must not be overridden by generic ones.
func finishReasonRank(reason string) int {
	switch reason {
	case "length", "content_filter":
		return 2
	case "tool_calls", "function_call":
		return 1
	default: // "stop" and anything else
		return 0
	}
}

// deferFinish records a finish reason for a choice; the strongest reason wins.
func (s *openAIInboundStream) deferFinish(index int, reason string) {
	if existing, ok := s.pendingFinish[index]; !ok || finishReasonRank(reason) > finishReasonRank(existing) {
		s.pendingFinish[index] = reason
	}
}

// choiceCarriesPayload reports whether the choice holds any client-visible
// payload (delta content, role marker, logprobs, ...) besides finish_reason.
func choiceCarriesPayload(choice *llm.Choice) bool {
	for _, message := range []*llm.Message{choice.Delta, choice.Message} {
		if message == nil {
			continue
		}
		if message.Role != "" ||
			len(message.ToolCalls) > 0 ||
			len(message.Content.MultipleContent) > 0 ||
			message.Content.Content != nil ||
			(message.ReasoningContent != nil && *message.ReasoningContent != "") ||
			(message.Reasoning != nil && *message.Reasoning != "") ||
			len(message.ReasoningItems) > 0 ||
			len(message.Annotations) > 0 ||
			message.Refusal != "" ||
			message.Audio != nil {
			return true
		}
	}

	return choice.Logprobs != nil
}

// responseCarriesPayload reports whether the response still holds anything
// worth emitting after finish reasons (and maybe usage) were stripped.
func responseCarriesPayload(resp *llm.Response) bool {
	if resp.Error != nil || resp.Usage != nil {
		return true
	}
	for i := range resp.Choices {
		if choiceCarriesPayload(&resp.Choices[i]) {
			return true
		}
	}
	return false
}

// takeFinishFrames drains deferred finish reasons into standalone terminal
// frames ordered by choice index, each with an empty delta. Choices for which
// verify returns false are dropped (e.g. incomplete tool arguments on error).
func (s *openAIInboundStream) takeFinishFrames(verify func(index int) bool) []*llm.Response {
	if len(s.pendingFinish) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(s.pendingFinish))
	for index := range s.pendingFinish {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	frames := make([]*llm.Response, 0, len(indexes))
	for _, index := range indexes {
		if !verify(index) {
			continue
		}
		reason := s.pendingFinish[index]
		frames = append(frames, &llm.Response{
			ID:      s.id,
			Model:   s.model,
			Created: s.created,
			Object:  "chat.completion.chunk",
			Choices: []llm.Choice{{Index: index, Delta: &llm.Message{}, FinishReason: &reason}},
		})
	}

	s.pendingFinish = make(map[int]string)

	return frames
}

// acceptAllChoices is the stream-end verifier: every deferred finish is valid.
func acceptAllChoices(int) bool { return true }

// verifyChoiceComplete is the error-path verifier: a finish is only delivered
// when the choice's aggregated output is complete, mirroring the clean-EOF
// synthesis conditions (non-empty output, well-formed tool call arguments).
func (s *openAIInboundStream) verifyChoiceComplete(index int) bool {
	state, ok := s.choices[index]
	if !ok || !state.output {
		return false
	}
	for _, tool := range state.tools {
		if strings.TrimSpace(tool.Function.Name) == "" || !json.Valid([]byte(tool.Function.Arguments)) {
			return false
		}
	}
	return true
}

// takePendingUsage returns a standalone usage frame for a held usage payload.
func (s *openAIInboundStream) takePendingUsage() *llm.Response {
	if s.pendingUsage == nil {
		return nil
	}
	resp := &llm.Response{
		ID:      s.id,
		Model:   s.model,
		Created: s.created,
		Object:  "chat.completion.chunk",
		Choices: []llm.Choice{},
		Usage:   s.pendingUsage,
	}
	s.pendingUsage = nil
	return resp
}

// synthesizeFinish synthesizes terminal frames for streams that ended without
// an upstream finish_reason (clean EOF with usage). The caller appends the
// [DONE] marker when synthesis actually happened.
func (s *openAIInboundStream) synthesizeFinish() []*llm.Response {
	if s.done || !s.usage {
		return nil
	}

	indexes := make([]int, 0, len(s.choices))
	for index, choice := range s.choices {
		if !choice.output {
			return nil
		}
		for _, tool := range choice.tools {
			if strings.TrimSpace(tool.Function.Name) == "" || !json.Valid([]byte(tool.Function.Arguments)) {
				return nil
			}
		}
		if !choice.finished {
			indexes = append(indexes, index)
		}
	}
	if len(indexes) == 0 {
		return nil
	}
	sort.Ints(indexes)
	choices := make([]llm.Choice, 0, len(indexes))
	for _, index := range indexes {
		reason := "stop"
		if len(s.choices[index].tools) > 0 {
			reason = "tool_calls"
		}
		choices = append(choices, llm.Choice{Index: index, Delta: &llm.Message{}, FinishReason: &reason})
	}
	finish := &llm.Response{ID: s.id, Model: s.model, Created: s.created, Object: "chat.completion.chunk", Choices: choices}
	finish.StreamCompletionEvidence = llm.StreamCompletionEvidenceOpenAIChatEOF
	return []*llm.Response{finish}
}
