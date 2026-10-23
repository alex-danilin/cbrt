package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Size caps for assembled stream state.
const (
	maxToolArgs   = 1 << 20 // per tool call
	maxAssembled  = 8 << 20 // text + reasoning + refusal + arguments + tool names/ids per response
	maxToolCalls  = 256     // distinct tool calls per response
	maxToolMeta   = 256     // bytes of a tool call's name or id
	thinkingBlk   = "thinking"
	textBlk       = "text"
	toolBlk       = "tool_use"
	callPending   = 0 // seen, not yet opened (arguments buffered)
	callOpen      = 1 // its block is open and streams live
	callEmitted   = 2 // its block is closed
	defaultChoice = 0
)

// Event is one SSE event: "event: <Name>\ndata: <Data>\n\n".
type Event struct {
	Name string
	Data []byte
}

// Bytes returns the event in SSE wire format.
func (e Event) Bytes() []byte {
	b := make([]byte, 0, len(e.Name)+len(e.Data)+16)
	b = append(b, "event: "...)
	b = append(b, e.Name...)
	b = append(b, "\ndata: "...)
	b = append(b, e.Data...)
	return append(b, "\n\n"...)
}

// Dropped kinds reported by StreamEncoder.Dropped.
const DroppedLateReasoning = "late_reasoning"

type streamCall struct {
	index   int
	upID    string // id as sent upstream ("" if it has sent none yet)
	id      string
	name    string
	args    []byte // all arguments, for validation at Finish
	pending []byte // arguments not yet sent to the client
	state   int
	block   int
}

// StreamEncoder converts upstream chat.completion.chunk objects into
// Anthropic Messages SSE events.
//
// Blocks are strictly sequential and never reopened. Reasoning that arrives
// after any text or tool_use block was opened is dropped (a thinking block
// may only lead the message) and reported by Dropped. An upstream refusal is
// sent as text (Anthropic has no refusal block). Only the currently open tool_use block streams
// input_json_delta live; arguments of other tool calls are buffered and
// emitted when that call can be opened, or at Finish in index order.
type StreamEncoder struct {
	model    string
	id       string
	inputEst int
	expose   bool

	next     int    // next block index
	open     string // type of the open block, "" if none
	openCall *streamCall
	calls    map[int]*streamCall
	dropped  []string
	late     bool // a text or tool_use block has been opened
	hasText  bool
	hasTool  bool
	finish   string
	usage    *chatUsage
	total    int
	done     bool
}

// NewStreamEncoder returns an encoder. inputTokens is the gateway's estimate,
// used in message_start (the real count arrives at the end).
func NewStreamEncoder(model string, inputTokens int, exposeReasoning bool) *StreamEncoder {
	return &StreamEncoder{
		model: model, id: randomID("msg_"), inputEst: inputTokens,
		expose: exposeReasoning, calls: map[int]*streamCall{},
	}
}

// Start returns the message_start event.
func (e *StreamEncoder) Start() []Event {
	msg := outMessage{
		ID: e.id, Type: "message", Role: "assistant", Model: e.model, Content: []any{},
		Usage: outUsage{InputTokens: e.inputEst},
	}
	return []Event{event("message_start", struct {
		Type    string     `json:"type"`
		Message outMessage `json:"message"`
	}{"message_start", msg})}
}

type chatChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          *string            `json:"content"`
			Refusal          *string            `json:"refusal"`
			ReasoningContent *string            `json:"reasoning_content"`
			Reasoning        *string            `json:"reasoning"`
			ToolCalls        []chatRespToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// Chunk consumes one upstream chat.completion.chunk JSON object.
func (e *StreamEncoder) Chunk(data []byte) ([]Event, error) {
	if e.done {
		return nil, errors.New("stream already finished")
	}
	var c chatChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("invalid upstream chunk: %w", err)
	}
	if !isNull(c.Error) {
		return nil, fmt.Errorf("upstream error: %s", compactJSON(c.Error))
	}
	if c.Usage != nil {
		e.usage = c.Usage
	}
	var ev []Event
	for _, ch := range c.Choices {
		if ch.Index != defaultChoice {
			continue
		}
		d := ch.Delta
		if r := pickReasoning(d.ReasoningContent, d.Reasoning); r != "" {
			if err := e.grow(len(r)); err != nil {
				return nil, err
			}
			ev = e.reasoning(ev, r)
		}
		if d.Content != nil && *d.Content != "" {
			if err := e.grow(len(*d.Content)); err != nil {
				return nil, err
			}
			ev = e.text(ev, *d.Content)
		}
		if d.Refusal != nil && *d.Refusal != "" {
			if err := e.grow(len(*d.Refusal)); err != nil {
				return nil, err
			}
			ev = e.text(ev, *d.Refusal)
		}
		for _, tc := range d.ToolCalls {
			var err error
			if ev, err = e.toolDelta(ev, tc); err != nil {
				return nil, err
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			e.finish = *ch.FinishReason
		}
	}
	return ev, nil
}

func (e *StreamEncoder) grow(n int) error {
	e.total += n
	if e.total > maxAssembled {
		return fmt.Errorf("assembled response exceeds %d bytes", maxAssembled)
	}
	return nil
}

// Dropped returns the kinds of upstream output that were dropped rather than
// sent (e.g. DroppedLateReasoning), deduplicated, in first-seen order.
func (e *StreamEncoder) Dropped() []string { return e.dropped }

func (e *StreamEncoder) drop(kind string) {
	for _, k := range e.dropped {
		if k == kind {
			return
		}
	}
	e.dropped = append(e.dropped, kind)
}

func (e *StreamEncoder) reasoning(ev []Event, r string) []Event {
	if !e.expose {
		return ev
	}
	if e.late {
		e.drop(DroppedLateReasoning)
		return ev
	}
	if e.open != thinkingBlk {
		ev = e.startBlock(ev, thinkingBlk, thinkingBlock{"thinking", "", ""})
	}
	return append(ev, e.delta(struct {
		Type     string `json:"type"`
		Thinking string `json:"thinking"`
	}{"thinking_delta", r}))
}

func (e *StreamEncoder) text(ev []Event, t string) []Event {
	e.late = true
	e.hasText = true
	if e.open != textBlk {
		ev = e.startBlock(ev, textBlk, textBlock{"text", ""})
	}
	return append(ev, e.textDelta(t))
}

func (e *StreamEncoder) toolDelta(ev []Event, tc chatRespToolCall) ([]Event, error) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	args, err := argString(tc.Function.Arguments)
	if err != nil {
		return nil, err
	}
	name := tc.Function.Name
	if len(name) > maxToolMeta || len(tc.ID) > maxToolMeta {
		return nil, fmt.Errorf("tool call %d: name or id exceeds %d bytes", idx, maxToolMeta)
	}
	c := e.calls[idx]
	if c == nil {
		if name == "" {
			return nil, fmt.Errorf("tool call %d: first chunk has no function name", idx)
		}
		if len(e.calls) >= maxToolCalls {
			return nil, fmt.Errorf("response has more than %d tool calls", maxToolCalls)
		}
		if err := e.grow(len(name) + len(tc.ID)); err != nil {
			return nil, err
		}
		id := tc.ID
		if id == "" {
			id = randomID("toolu_")
		}
		c = &streamCall{index: idx, upID: tc.ID, id: id, name: name, block: -1}
		e.calls[idx] = c
	} else {
		// Some providers repeat the name and id on every chunk; a different
		// value would silently re-target the call.
		if name != "" && name != c.name {
			return nil, fmt.Errorf("tool call %d: name changed mid-stream", idx)
		}
		switch {
		case tc.ID == "" || tc.ID == c.upID:
		case c.upID != "":
			return nil, fmt.Errorf("tool call %d: id changed mid-stream", idx)
		case c.state == callPending:
			// The first id arrived after the name: adopt it while the client
			// has not seen the generated one.
			if err := e.grow(len(tc.ID)); err != nil {
				return nil, err
			}
			c.upID, c.id = tc.ID, tc.ID
		default:
			// The client already has the generated id; ids are opaque and
			// only need to round-trip through the client, so it stays.
			c.upID = tc.ID
		}
	}
	if len(c.args)+len(args) > maxToolArgs {
		return nil, fmt.Errorf("tool call %q: arguments exceed %d bytes", c.name, maxToolArgs)
	}
	if err := e.grow(len(args)); err != nil {
		return nil, err
	}
	c.args = append(c.args, args...)

	switch c.state {
	case callOpen:
		if args != "" {
			ev = append(ev, e.jsonDelta(args))
		}
	case callEmitted:
		if args != "" {
			return nil, fmt.Errorf("tool call %q: arguments arrived after its block was closed", c.name)
		}
	case callPending:
		if e.open == toolBlk {
			c.pending = append(c.pending, args...)
			break
		}
		ev = e.openTool(ev, c)
		if len(c.pending) > 0 {
			ev = append(ev, e.jsonDelta(string(c.pending)))
			c.pending = nil
		}
		if args != "" {
			ev = append(ev, e.jsonDelta(args))
		}
	}
	return ev, nil
}

func (e *StreamEncoder) openTool(ev []Event, c *streamCall) []Event {
	e.late = true
	e.hasTool = true
	ev = e.startBlock(ev, toolBlk, toolUseBlock{"tool_use", c.id, c.name, json.RawMessage("{}")})
	c.state = callOpen
	c.block = e.next - 1
	e.openCall = c
	return ev
}

// Finish handles upstream [DONE]: it validates tool arguments, closes and
// flushes blocks and returns message_delta and message_stop. On error no
// events are returned; the caller should send Fail.
func (e *StreamEncoder) Finish() ([]Event, error) {
	if e.done {
		return nil, errors.New("stream already finished")
	}
	idxs := make([]int, 0, len(e.calls))
	for i := range e.calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		c := e.calls[i]
		if _, err := parseArgs(c.name, string(c.args)); err != nil {
			return nil, err
		}
	}
	e.done = true

	ev := e.closeBlock(nil)
	for _, i := range idxs {
		c := e.calls[i]
		if c.state != callPending {
			continue
		}
		ev = e.openTool(ev, c)
		if len(c.pending) > 0 {
			ev = append(ev, e.jsonDelta(string(c.pending)))
			c.pending = nil
		}
		ev = e.closeBlock(ev)
	}
	if !e.hasText && !e.hasTool {
		ev = e.startBlock(ev, textBlk, textBlock{"text", ""})
		ev = e.closeBlock(ev)
	}

	stop := stopReason(e.finish, e.hasTool)
	type delta struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	}
	ev = append(ev, event("message_delta", struct {
		Type  string   `json:"type"`
		Delta delta    `json:"delta"`
		Usage outUsage `json:"usage"`
	}{"message_delta", delta{StopReason: stop}, convertUsage(e.usage, e.inputEst, estimateTokens(e.total))}))
	ev = append(ev, event("message_stop", struct {
		Type string `json:"type"`
	}{"message_stop"}))
	return ev, nil
}

// Fail returns an error event. It closes nothing; the stream ends after it.
func (e *StreamEncoder) Fail(errType, msg string) []Event {
	e.done = true
	return []Event{{Name: "error", Data: ErrorJSON(errType, msg)}}
}

func (e *StreamEncoder) startBlock(ev []Event, kind string, block any) []Event {
	ev = e.closeBlock(ev)
	ev = append(ev, event("content_block_start", struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock any    `json:"content_block"`
	}{"content_block_start", e.next, block}))
	e.open = kind
	e.next++
	return ev
}

func (e *StreamEncoder) closeBlock(ev []Event) []Event {
	if e.open == "" {
		return ev
	}
	if e.openCall != nil {
		e.openCall.state = callEmitted
		e.openCall = nil
	}
	e.open = ""
	return append(ev, event("content_block_stop", struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}{"content_block_stop", e.next - 1}))
}

func (e *StreamEncoder) delta(d any) Event {
	return event("content_block_delta", struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta any    `json:"delta"`
	}{"content_block_delta", e.next - 1, d})
}

func (e *StreamEncoder) textDelta(t string) Event {
	return e.delta(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"text_delta", t})
}

func (e *StreamEncoder) jsonDelta(s string) Event {
	return e.delta(struct {
		Type        string `json:"type"`
		PartialJSON string `json:"partial_json"`
	}{"input_json_delta", s})
}

func event(name string, v any) Event { return Event{Name: name, Data: marshal(v)} }
