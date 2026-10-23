package responses

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Size caps for assembled stream state.
const (
	maxToolArgs   = 1 << 20 // per tool call
	maxAssembled  = 8 << 20 // text + reasoning + refusal + arguments + tool names/ids per response
	maxToolCalls  = 256     // distinct tool calls per response
	maxToolMeta   = 256     // bytes of a tool call's name or id
	defaultChoice = 0
)

// Content part kinds of a message item.
const (
	partText    = "output_text"
	partRefusal = "refusal"
)

// Dropped kinds reported by StreamEncoder.Dropped.
const DroppedLateReasoning = "late_reasoning"

// Open item kinds.
const (
	kindNone      = ""
	kindReasoning = "reasoning"
	kindMessage   = "message"
	kindFunction  = "function_call"
	kindCustom    = "custom_tool_call"
)

// Tool call states.
const (
	callPending = iota // seen, not yet opened (arguments buffered)
	callOpen           // its item is open
	callEmitted        // its item is closed
)

// Event is one SSE event: "event: <Name>\ndata: <Data>\n\n". Data is a JSON
// object carrying "type" (= Name) and "sequence_number".
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

type streamCall struct {
	index    int
	upID     string // id as sent upstream ("" if it has sent none yet)
	callID   string
	chatName string
	ref      toolRef
	args     []byte // all arguments, for validation and the final item
	pending  []byte // arguments not yet sent to the client (function calls)
	sent     bool   // an arguments delta was sent
	state    int
	itemID   string
}

// StreamEncoder converts upstream chat.completion.chunk objects into
// Responses SSE events.
//
// Output items are strictly sequential; opening an item closes the open
// one. Reasoning that arrives after any message or tool call item was opened
// is dropped (a reasoning item may only lead the output) and reported by
// Dropped. A message item holds output_text and refusal parts in arrival
// order; a change of kind closes the open part and opens the next. Only the currently
// open function_call streams argument deltas live; arguments of other calls
// are buffered and emitted when that call can be opened, or at Finish in
// index order. Custom tool calls are accumulated (their arguments are a
// JSON-wrapped string) and emitted in full when their item closes. The
// assembled output is kept for response.completed.
type StreamEncoder struct {
	model    string
	id       string
	created  int64
	inputEst int
	tools    ToolMap
	expose   bool

	seq      int
	output   []any  // closed items
	open     string // kind of the open item
	openID   string
	openText []byte // text of the open message part or reasoning item
	openPart string // kind of the open message part
	parts    []any  // closed parts of the open message
	openCall *streamCall
	dropped  []string
	calls    map[int]*streamCall
	late     bool // a message or tool call item has been opened
	hasText  bool
	hasTool  bool
	finish   string
	usage    *chatUsage
	total    int
	done     bool
}

// NewStreamEncoder returns an encoder. inputTokens is the gateway's estimate,
// used when upstream reports no usage. tools is RequestInfo.Tools.
func NewStreamEncoder(model string, inputTokens int, tools ToolMap, exposeReasoning bool) *StreamEncoder {
	return &StreamEncoder{
		model: model, id: randomID("resp_"), created: time.Now().Unix(), inputEst: inputTokens,
		tools: tools, expose: exposeReasoning, calls: map[int]*streamCall{},
	}
}

// Start returns response.created and response.in_progress.
func (e *StreamEncoder) Start() []Event {
	r := newResponse(e.id, e.model, e.created)
	return []Event{
		e.event("response.created", map[string]any{"response": r}),
		e.event("response.in_progress", map[string]any{"response": r}),
	}
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
			var err error
			if ev, err = e.part(ev, partText, *d.Content); err != nil {
				return nil, err
			}
		}
		if d.Refusal != nil && *d.Refusal != "" {
			if err := e.grow(len(*d.Refusal)); err != nil {
				return nil, err
			}
			var err error
			if ev, err = e.part(ev, partRefusal, *d.Refusal); err != nil {
				return nil, err
			}
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

func (e *StreamEncoder) outputIndex() int { return len(e.output) }

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
	if e.open != kindReasoning {
		// Nothing but reasoning can be open here.
		e.open, e.openID, e.openText = kindReasoning, randomID("rs_"), nil
		idx := e.outputIndex()
		ev = append(ev,
			e.event("response.output_item.added", map[string]any{"output_index": idx,
				"item": reasoningItem{Type: "reasoning", ID: e.openID, Summary: []summaryText{}}}),
			e.event("response.reasoning_summary_part.added", map[string]any{"item_id": e.openID,
				"output_index": idx, "summary_index": 0, "part": summaryText{"summary_text", ""}}))
	}
	e.openText = append(e.openText, r...)
	return append(ev, e.event("response.reasoning_summary_text.delta", map[string]any{
		"item_id": e.openID, "output_index": e.outputIndex(), "summary_index": 0, "delta": r}))
}

// part appends t to a message part of the given kind (partText or
// partRefusal), opening the message item or switching parts as needed.
func (e *StreamEncoder) part(ev []Event, kind, t string) ([]Event, error) {
	e.late = true
	e.hasText = true
	switch {
	case e.open != kindMessage:
		var err error
		if ev, err = e.openMessage(ev, kind); err != nil {
			return nil, err
		}
	case e.openPart != kind:
		ev = e.closePart(ev)
		ev = e.startPart(ev, kind)
	}
	e.openText = append(e.openText, t...)
	fields := map[string]any{"item_id": e.openID, "output_index": e.outputIndex(), "content_index": len(e.parts), "delta": t}
	if kind == partRefusal {
		return append(ev, e.event("response.refusal.delta", fields)), nil
	}
	fields["logprobs"] = []any{}
	return append(ev, e.event("response.output_text.delta", fields)), nil
}

// openMessage closes the open item and opens an empty message item with an
// empty part of the given kind.
func (e *StreamEncoder) openMessage(ev []Event, kind string) ([]Event, error) {
	var err error
	if ev, err = e.closeItem(ev); err != nil {
		return nil, err
	}
	e.open, e.openID, e.parts = kindMessage, randomID("msg_"), nil
	ev = append(ev, e.event("response.output_item.added", map[string]any{"output_index": e.outputIndex(),
		"item": messageItem{Type: "message", ID: e.openID, Status: "in_progress", Role: "assistant", Content: []any{}}}))
	return e.startPart(ev, kind), nil
}

// startPart opens an empty content part in the open message.
func (e *StreamEncoder) startPart(ev []Event, kind string) []Event {
	e.openPart, e.openText = kind, nil
	return append(ev, e.event("response.content_part.added", map[string]any{"item_id": e.openID,
		"output_index": e.outputIndex(), "content_index": len(e.parts), "part": newPart(kind, "")}))
}

// closePart emits the done events of the open content part and keeps it.
func (e *StreamEncoder) closePart(ev []Event) []Event {
	text := string(e.openText)
	p := newPart(e.openPart, text)
	fields := map[string]any{"item_id": e.openID, "output_index": e.outputIndex(), "content_index": len(e.parts)}
	if e.openPart == partRefusal {
		fields["refusal"] = text
		ev = append(ev, e.event("response.refusal.done", fields))
	} else {
		fields["text"], fields["logprobs"] = text, []any{}
		ev = append(ev, e.event("response.output_text.done", fields))
	}
	ev = append(ev, e.event("response.content_part.done", map[string]any{"item_id": e.openID,
		"output_index": e.outputIndex(), "content_index": len(e.parts), "part": p}))
	e.parts = append(e.parts, p)
	e.openPart, e.openText = "", nil
	return ev
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
			id = randomID("call_")
		}
		c = &streamCall{index: idx, upID: tc.ID, callID: id, chatName: name, ref: e.tools.lookup(name)}
		e.calls[idx] = c
	} else {
		// Some providers repeat the name and id on every chunk; a different
		// value would silently re-target the call.
		if name != "" && name != c.chatName {
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
			c.upID, c.callID = tc.ID, tc.ID
		default:
			// The client already has the generated id; ids are opaque and
			// only need to round-trip through the client, so it stays.
			c.upID = tc.ID
		}
	}
	if len(c.args)+len(args) > maxToolArgs {
		return nil, fmt.Errorf("tool call %q: arguments exceed %d bytes", c.ref.name, maxToolArgs)
	}
	if err := e.grow(len(args)); err != nil {
		return nil, err
	}
	c.args = append(c.args, args...)

	switch c.state {
	case callOpen:
		if args != "" && !c.ref.custom {
			ev = append(ev, e.argsDelta(c, args))
		}
	case callEmitted:
		if args != "" {
			return nil, fmt.Errorf("tool call %q: arguments arrived after its item was closed", c.ref.name)
		}
	case callPending:
		if !c.ref.custom {
			c.pending = append(c.pending, args...)
		}
		if e.open == kindFunction || e.open == kindCustom {
			break
		}
		if ev, err = e.openTool(ev, c); err != nil {
			return nil, err
		}
	}
	return ev, nil
}

// openTool closes the open item and opens c. A function call streams its
// buffered arguments; a custom call emits nothing until it closes.
func (e *StreamEncoder) openTool(ev []Event, c *streamCall) ([]Event, error) {
	var err error
	if ev, err = e.closeItem(ev); err != nil {
		return nil, err
	}
	e.late = true
	e.hasTool = true
	c.state = callOpen
	e.openCall = c
	if c.ref.custom {
		c.itemID = randomID("ctc_")
		e.open, e.openID = kindCustom, c.itemID
		return ev, nil
	}
	c.itemID = randomID("fc_")
	e.open, e.openID = kindFunction, c.itemID
	ev = append(ev, e.event("response.output_item.added", map[string]any{"output_index": e.outputIndex(),
		"item": functionCallItem{Type: "function_call", ID: c.itemID, CallID: c.callID, Name: c.ref.name,
			Namespace: c.ref.namespace, Arguments: "", Status: "in_progress"}}))
	if len(c.pending) > 0 {
		ev = append(ev, e.argsDelta(c, string(c.pending)))
		c.pending = nil
	}
	return ev, nil
}

func (e *StreamEncoder) argsDelta(c *streamCall, s string) Event {
	c.sent = true
	return e.event("response.function_call_arguments.delta", map[string]any{
		"item_id": c.itemID, "output_index": e.outputIndex(), "delta": s})
}

// closeItem closes the open item, emitting its done events and adding it to
// the assembled output. Closing a custom tool call validates its input.
func (e *StreamEncoder) closeItem(ev []Event) ([]Event, error) {
	idx := e.outputIndex()
	var item any
	switch e.open {
	case kindNone:
		return ev, nil
	case kindReasoning:
		text := string(e.openText)
		ev = append(ev,
			e.event("response.reasoning_summary_text.done", map[string]any{"item_id": e.openID,
				"output_index": idx, "summary_index": 0, "text": text}),
			e.event("response.reasoning_summary_part.done", map[string]any{"item_id": e.openID,
				"output_index": idx, "summary_index": 0, "part": summaryText{"summary_text", text}}))
		item = newReasoning(e.openID, text)
	case kindMessage:
		ev = e.closePart(ev)
		item = messageItem{Type: "message", ID: e.openID, Status: "completed", Role: "assistant", Content: e.parts}
		e.parts = nil
	case kindFunction:
		c := e.openCall
		args, err := functionArgs(c.ref.name, string(c.args))
		if err != nil {
			return nil, err
		}
		if !c.sent {
			ev = append(ev, e.argsDelta(c, args))
		}
		ev = append(ev, e.event("response.function_call_arguments.done", map[string]any{
			"item_id": c.itemID, "output_index": idx, "arguments": args}))
		item = functionCallItem{Type: "function_call", ID: c.itemID, CallID: c.callID, Name: c.ref.name,
			Namespace: c.ref.namespace, Arguments: args, Status: "completed"}
	case kindCustom:
		c := e.openCall
		input, err := customInput(c.ref.name, string(c.args))
		if err != nil {
			return nil, err
		}
		added := customCallItem{Type: "custom_tool_call", ID: c.itemID, CallID: c.callID, Name: c.ref.name,
			Namespace: c.ref.namespace, Input: "", Status: "in_progress"}
		ev = append(ev,
			e.event("response.output_item.added", map[string]any{"output_index": idx, "item": added}),
			e.event("response.custom_tool_call_input.delta", map[string]any{
				"item_id": c.itemID, "output_index": idx, "delta": input}),
			e.event("response.custom_tool_call_input.done", map[string]any{
				"item_id": c.itemID, "output_index": idx, "input": input}))
		done := added
		done.Input, done.Status = input, "completed"
		item = done
	}
	if e.openCall != nil {
		e.openCall.state = callEmitted
		e.openCall = nil
	}
	e.open, e.openID, e.openText = kindNone, "", nil
	e.output = append(e.output, item)
	return append(ev, e.event("response.output_item.done", map[string]any{"output_index": idx, "item": item})), nil
}

// Finish handles upstream [DONE]: it validates tool arguments, closes and
// flushes items and returns response.completed (or response.incomplete when
// upstream stopped on length). On error no events are returned; the caller
// should send Fail.
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
		if c.state == callEmitted {
			continue
		}
		var err error
		if c.ref.custom {
			_, err = customInput(c.ref.name, string(c.args))
		} else {
			_, err = functionArgs(c.ref.name, string(c.args))
		}
		if err != nil {
			return nil, err
		}
	}
	e.done = true

	ev, err := e.closeItem(nil)
	if err != nil {
		return nil, err
	}
	for _, i := range idxs {
		c := e.calls[i]
		if c.state != callPending {
			continue
		}
		if ev, err = e.openTool(ev, c); err != nil {
			return nil, err
		}
		if ev, err = e.closeItem(ev); err != nil {
			return nil, err
		}
	}
	if !e.hasText && !e.hasTool {
		if ev, err = e.openMessage(ev, partText); err != nil {
			return nil, err
		}
		if ev, err = e.closeItem(ev); err != nil {
			return nil, err
		}
	}

	r := newResponse(e.id, e.model, e.created)
	r.Output = e.output
	r.Status, r.IncompleteDetails = finishStatus(e.finish)
	u := convertUsage(e.usage, e.inputEst, estimateTokens(e.total))
	r.Usage = &u
	name := "response.completed"
	if r.Status == "incomplete" {
		name = "response.incomplete"
	}
	return append(ev, e.event(name, map[string]any{"response": r})), nil
}

// Fail returns a response.failed event carrying error {code, message} and the
// output assembled so far. It closes nothing; the stream ends after it.
func (e *StreamEncoder) Fail(code, msg string) []Event {
	e.done = true
	r := newResponse(e.id, e.model, e.created)
	r.Output = e.output
	r.Status = "failed"
	r.Error = map[string]string{"code": code, "message": msg}
	return []Event{e.event("response.failed", map[string]any{"response": r})}
}

// event builds an event whose data is fields plus type and sequence_number.
func (e *StreamEncoder) event(name string, fields map[string]any) Event {
	fields["type"] = name
	fields["sequence_number"] = e.seq
	e.seq++
	return Event{Name: name, Data: marshal(fields)}
}
