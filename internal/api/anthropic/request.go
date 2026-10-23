package anthropic

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/alex-danilin/cbrt/internal/jsonscan"
)

// RequestInfo describes a decoded Anthropic request.
type RequestInfo struct {
	Model           string   // model as requested by the client, e.g. "claude-sonnet-4-5"
	Stream          bool     // client asked for SSE
	Downgraded      []string // what was dropped/approximated, deduplicated, in first-seen order
	ReasoningEffort string   // output_config.effort if present, else ""
}

// billingPrefix marks Claude Code's pseudo system block.
const billingPrefix = "x-anthropic-billing-header:"

// The decoder validates the body once and then works on it without copying
// or re-parsing: every []byte field below is a slice of the body holding one
// raw JSON value (string fields keep their quoted literal), and the Chat
// Completions body is appended directly to an output buffer. Large values
// (tool input schemas, message texts, tool results) are copied to the
// output as they are and never decoded. Objects are matched with
// encoding/json's rules (see package jsonscan), so the result is the same as
// decoding into the equivalent structs.

// inMessage is the envelope of a message. (CountTokens reports decoding
// errors in terms of this type.)
type inMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// message is a decoded inMessage; content is any JSON value or nil.
type message struct {
	role    string
	content []byte
}

func decodeMessage(x *jsonscan.Index, m *message, v []byte) bool {
	if v[0] != '{' {
		return false
	}
	var kb [32]byte
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := it.Next()
		if !more {
			return true
		}
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "role":
			if !fieldEnum(val, &m.role) {
				return false
			}
		case "content":
			m.content = val
		}
	}
}

// inBlock is a content block.
type inBlock struct {
	typ, id, name, toolUseID string
	text                     []byte // string literal or nil
	input, content           []byte // any JSON value or nil
	cacheControl             []byte // any JSON value or nil
	isError                  bool
	hasSource                bool // source is a non-null object
	source                   imageSource
}

type imageSource struct {
	typ                  string
	mediaType, data, url []byte // string literals or nil
}

func decodeBlock(x *jsonscan.Index, b *inBlock, v []byte) bool {
	if v[0] != '{' {
		return false
	}
	var kb [32]byte
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := it.Next()
		if !more {
			return true
		}
		ok := true
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "type":
			ok = fieldEnum(val, &b.typ)
		case "text":
			ok = jsonscan.FieldRawStr(val, &b.text)
		case "source":
			switch val[0] {
			case 'n':
				b.hasSource = false
			case '{':
				if !b.hasSource {
					b.hasSource, b.source = true, imageSource{}
				}
				ok = decodeSource(x, &b.source, val)
			default:
				ok = false
			}
		case "id":
			ok = jsonscan.FieldStr(val, &b.id)
		case "name":
			ok = jsonscan.FieldStr(val, &b.name)
		case "input":
			b.input = val
		case "tool_use_id":
			ok = jsonscan.FieldStr(val, &b.toolUseID)
		case "content":
			b.content = val
		case "is_error":
			ok = jsonscan.FieldBool(val, &b.isError)
		case "cache_control":
			b.cacheControl = val
		}
		if !ok {
			return false
		}
	}
}

func decodeSource(x *jsonscan.Index, s *imageSource, v []byte) bool {
	var kb [32]byte
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := it.Next()
		if !more {
			return true
		}
		ok := true
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "type":
			ok = fieldEnum(val, &s.typ)
		case "media_type":
			ok = jsonscan.FieldRawStr(val, &s.mediaType)
		case "data":
			ok = jsonscan.FieldRawStr(val, &s.data)
		case "url":
			ok = jsonscan.FieldRawStr(val, &s.url)
		}
		if !ok {
			return false
		}
	}
}

// inTool is a tool definition.
type inTool struct {
	typ, name    string
	desc         []byte // string literal or nil
	inputSchema  []byte // any JSON value or nil
	cacheControl []byte // any JSON value or nil
	strict       bool   // "strict": true (dropped: free providers reject it)
}

func decodeTool(x *jsonscan.Index, t *inTool, v []byte) bool {
	if v[0] != '{' {
		return false
	}
	var kb [32]byte
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, more := it.Next()
		if !more {
			return true
		}
		ok := true
		switch string(jsonscan.FoldKey(&kb, k)) {
		case "type":
			ok = fieldEnum(val, &t.typ)
		case "name":
			ok = jsonscan.FieldStr(val, &t.name)
		case "description":
			ok = jsonscan.FieldRawStr(val, &t.desc)
		case "input_schema":
			t.inputSchema = val
		case "cache_control":
			t.cacheControl = val
		case "strict":
			t.strict = string(val) == "true"
		}
		if !ok {
			return false
		}
	}
}

// fieldEnum is jsonscan.FieldStr for fields with a small set of common values,
// which are returned without allocating.
func fieldEnum(v []byte, dst *string) bool {
	switch string(v) {
	case `"text"`:
		*dst = "text"
	case `"user"`:
		*dst = "user"
	case `"assistant"`:
		*dst = "assistant"
	case `"system"`:
		*dst = "system"
	case `"tool_use"`:
		*dst = "tool_use"
	case `"tool_result"`:
		*dst = "tool_result"
	case `"thinking"`:
		*dst = "thinking"
	case `"image"`:
		*dst = "image"
	case `"base64"`:
		*dst = "base64"
	case `"custom"`:
		*dst = "custom"
	default:
		return jsonscan.FieldStr(v, dst)
	}
	return true
}

// emptyStr reports whether the string literal s (nil is "") is empty.
func emptyStr(s []byte) bool { return len(s) <= 2 }

// absentOrNull reports whether a raw value is absent or null.
func absentOrNull(v []byte) bool { return v == nil || jsonscan.IsNull(v) }

// topLevel holds the members of the request object with the semantics of
// map[string]json.RawMessage (exact keys, the last duplicate wins), in the
// order of first occurrence. Small objects are searched linearly; past
// linearTop members a key index keeps decoding linear in the member count.
type topLevel struct {
	members []topMember
	index   map[string]int // key -> position in members; nil while small
}

type topMember struct {
	key string
	val []byte
}

// linearTop is the member count up to which keys are matched by a scan.
const linearTop = 16

func splitTop(x *jsonscan.Index, v []byte) topLevel {
	top := topLevel{members: make([]topMember, 0, linearTop)}
	it := jsonscan.ObjectMembers(x, v)
	for {
		k, val, ok := it.Next()
		if !ok {
			return top
		}
		top.set(jsonscan.Unquote(k), val)
	}
}

func (t *topLevel) set(key string, val []byte) {
	if i, ok := t.find(key); ok {
		t.members[i].val = val
		return
	}
	if t.index != nil {
		t.index[key] = len(t.members)
	}
	t.members = append(t.members, topMember{key, val})
	if t.index == nil && len(t.members) > linearTop {
		t.index = make(map[string]int, 2*len(t.members))
		for i, m := range t.members {
			t.index[m.key] = i
		}
	}
}

func (t *topLevel) find(key string) (int, bool) {
	if t.index != nil {
		i, ok := t.index[key]
		return i, ok
	}
	for i := range t.members {
		if t.members[i].key == key {
			return i, true
		}
	}
	return 0, false
}

func (t *topLevel) get(key string) []byte {
	if i, ok := t.find(key); ok {
		return t.members[i].val
	}
	return nil
}

// null reports whether the member is absent or null.
func (t *topLevel) null(key string) bool { return absentOrNull(t.get(key)) }

// loc names a place in the request in errors: "system" (loc < 0) or
// "messages[i]".
type loc int

func (l loc) String() string {
	if l < 0 {
		return "system"
	}
	return fmt.Sprintf("messages[%d]", int(l))
}

type decoder struct {
	info RequestInfo
	seen map[string]bool
	x    *jsonscan.Index // of the request body

	out   []byte // the chat body being written
	nMsgs int    // messages written to out

	blocks []inBlock  // scratch
	nested []inBlock  // scratch for tool_result content
	texts  [][]byte   // scratch
	parts  []chatPart // scratch
}

func (d *decoder) downgrade(what string) {
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if !d.seen[what] {
		d.seen[what] = true
		d.info.Downgraded = append(d.info.Downgraded, what)
	}
}

func (d *decoder) noteCache(raw []byte) {
	if !absentOrNull(raw) {
		d.downgrade("cache_control")
	}
}

// DecodeRequest converts an Anthropic Messages request body into an OpenAI
// Chat Completions request body. It returns *Error for content that cannot
// be represented faithfully.
func DecodeRequest(body []byte) ([]byte, RequestInfo, error) {
	d := &decoder{x: jsonscan.NewIndex(body)}
	v := jsonscan.TrimSpace(body)
	if d.x == nil || v[0] != '{' && !jsonscan.IsNull(v) {
		var top map[string]json.RawMessage
		err := json.Unmarshal(body, &top) // for the error message
		return nil, d.info, errorf("invalid request body: %v", err)
	}
	var top topLevel
	if v[0] == '{' {
		top = splitTop(d.x, v)
	}

	model, ok := unmarshalString(top.get("model"))
	if !ok || model == "" {
		return nil, d.info, errorf("model: must be a non-empty string")
	}
	d.info.Model = model

	maxTokens, ok := jsonscan.ParseFloat(top.get("max_tokens"))
	if !ok || maxTokens < 1 || maxTokens != math.Trunc(maxTokens) || maxTokens > math.MaxInt32 {
		return nil, d.info, errorf("max_tokens: must be a positive integer")
	}

	if !top.null("stream") {
		if !jsonscan.FieldBool(top.get("stream"), &d.info.Stream) {
			return nil, d.info, errorf("stream: must be a boolean")
		}
	}

	var sampling [2][]byte // temperature, top_p (valid numbers, written as is)
	for i, k := range [...]string{"temperature", "top_p"} {
		if top.null(k) {
			continue
		}
		raw := top.get(k)
		if _, ok := jsonscan.ParseFloat(raw); !ok {
			return nil, d.info, errorf("%s: must be a number", k)
		}
		sampling[i] = raw
	}
	var stop []string
	if !top.null("stop_sequences") {
		if err := json.Unmarshal(top.get("stop_sequences"), &stop); err != nil {
			return nil, d.info, errorf("stop_sequences: must be an array of strings")
		}
	}

	rawMsgs := top.get("messages")
	var msgs []message
	if rawMsgs != nil && !jsonscan.IsNull(rawMsgs) {
		msgs, ok = jsonscan.DecodeSlice[message](d.x, nil, rawMsgs, true, decodeMessage)
	}
	if rawMsgs == nil || !ok || len(msgs) == 0 {
		return nil, d.info, errorf("messages: must be a non-empty array")
	}

	// The chat body is written into one buffer: the scalar fields, the
	// messages, then the tools (JSON member order is not significant).
	size := len(model) + len(top.get("system")) + len(rawMsgs) + len(top.get("tools"))
	d.out = make([]byte, 0, size+size/16+256) // the output is about as large
	d.out = append(d.out, `{"model":`...)
	d.out = jsonscan.AppendQuoted(d.out, model)
	d.out = append(d.out, `,"stream":`...)
	d.out = strconv.AppendBool(d.out, d.info.Stream)
	d.out = append(d.out, `,"max_tokens":`...)
	d.out = strconv.AppendInt(d.out, int64(maxTokens), 10)
	if sampling[0] != nil {
		d.out = append(d.out, `,"temperature":`...)
		d.out = append(d.out, sampling[0]...)
	}
	if sampling[1] != nil {
		d.out = append(d.out, `,"top_p":`...)
		d.out = append(d.out, sampling[1]...)
	}
	if len(stop) > 0 {
		d.out = append(d.out, `,"stop":`...)
		d.out = append(d.out, marshal(stop)...)
	}
	d.out = append(d.out, `,"messages":[`...)

	if !top.null("system") {
		if err := d.systemMessage(top.get("system"), -1, true); err != nil {
			return nil, d.info, err
		}
	}

	for i := range msgs {
		m := &msgs[i]
		var err error
		switch m.role {
		case "system":
			err = d.systemMessage(m.content, loc(i), false)
		case "user":
			err = d.userMessage(m.content, loc(i))
		case "assistant":
			err = d.assistantMessage(m.content, loc(i))
		default:
			err = errorf("%s: unsupported role %q", loc(i), m.role)
		}
		if err != nil {
			return nil, d.info, err
		}
	}
	d.out = append(d.out, ']')

	nTools := 0
	if !top.null("tools") {
		var err error
		if nTools, err = d.tools(top.get("tools")); err != nil {
			return nil, d.info, err
		}
	}
	if !top.null("tool_choice") {
		choice, noParallel, err := toolChoice(top.get("tool_choice"))
		if err != nil {
			return nil, d.info, err
		}
		// A tool_choice without tools is rejected by most upstreams.
		if nTools > 0 {
			d.out = append(d.out, `,"tool_choice":`...)
			d.out = append(d.out, marshal(choice)...)
			if noParallel {
				d.out = append(d.out, `,"parallel_tool_calls":false`...)
			}
		}
	}

	if !top.null("output_config") {
		d.downgrade("output_config")
		var oc struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(top.get("output_config"), &oc) == nil {
			d.info.ReasoningEffort = oc.Effort
		}
	}

	// Anthropic-only fields (thinking, metadata, context_management, top_k,
	// service_tier, container, mcp_servers, ...) and anything unrecognized
	// are dropped and reported by name.
	var unknown []string
	for _, m := range top.members {
		switch m.key {
		case "model", "max_tokens", "stream", "temperature", "top_p",
			"stop_sequences", "messages", "system", "tools", "tool_choice", "output_config":
			continue
		}
		if !jsonscan.IsNull(m.val) {
			unknown = append(unknown, m.key)
		}
	}
	sort.Strings(unknown)
	for _, k := range unknown {
		d.downgrade(k)
	}

	return append(d.out, '}'), d.info, nil
}

// unmarshalString decodes a raw value like json.Unmarshal into a string:
// absent is an error, null is "".
func unmarshalString(v []byte) (string, bool) {
	switch {
	case v == nil:
		return "", false
	case v[0] == '"':
		return jsonscan.Unquote(v), true
	case jsonscan.IsNull(v):
		return "", true
	}
	return "", false
}

// beginMsg starts a message; its content value must follow.
func (d *decoder) beginMsg(role string) {
	if d.nMsgs > 0 {
		d.out = append(d.out, ',')
	}
	d.nMsgs++
	d.out = append(d.out, `{"role":"`...)
	d.out = append(d.out, role...)
	d.out = append(d.out, `","content":`...)
}

// parseContent accepts a string or an array of blocks (like decoding into a
// string, then into []inBlock). The result is scratch space valid until the
// next call.
func (d *decoder) parseContent(raw []byte, where loc, scratch *[]inBlock) ([]inBlock, error) {
	switch {
	case raw == nil:
	case raw[0] == '"' || jsonscan.IsNull(raw):
		blocks := append((*scratch)[:0], inBlock{typ: "text", text: raw})
		if raw[0] == 'n' {
			blocks[0].text = nil
		}
		*scratch = blocks
		return blocks, nil
	default:
		if blocks, ok := jsonscan.DecodeSlice(d.x, *scratch, raw, true, decodeBlock); ok {
			*scratch = blocks
			return blocks, nil
		}
	}
	return nil, errorf("%s: content must be a string or an array of content blocks", where)
}

// systemMessage writes a system message: a system prompt (string or text
// blocks) flattened, dropping Claude Code's billing pseudo-header block. The
// top-level system prompt (top) is omitted when empty.
func (d *decoder) systemMessage(raw []byte, where loc, top bool) error {
	blocks, err := d.parseContent(raw, where, &d.blocks)
	if err != nil {
		return err
	}
	texts := d.texts[:0]
	for i := range blocks {
		b := &blocks[i]
		d.noteCache(b.cacheControl)
		if b.typ != "text" {
			return errorf("%s: unsupported content block type %q in system content", where, b.typ)
		}
		if hasBillingPrefix(b.text) {
			d.downgrade("billing_header")
			continue
		}
		texts = append(texts, b.text)
	}
	d.texts = texts
	if top && joinedEmpty(texts) {
		return nil
	}
	d.beginMsg("system")
	d.out = jsonscan.AppendJoined(d.out, texts, "\n\n")
	d.out = append(d.out, '}')
	return nil
}

// hasBillingPrefix reports whether the string literal s starts with
// billingPrefix once decoded.
func hasBillingPrefix(s []byte) bool {
	if len(s) < 2+len(billingPrefix) {
		return false
	}
	head := s[1 : 1+len(billingPrefix)]
	for _, c := range head {
		if c == '\\' {
			return strings.HasPrefix(jsonscan.Unquote(s), billingPrefix)
		}
	}
	// Without escapes the literal starts with the decoded text (a non-ASCII
	// byte cannot match the ASCII prefix either way).
	return string(head) == billingPrefix
}

// joinedEmpty reports whether texts joined by "\n\n" is empty.
func joinedEmpty(texts [][]byte) bool {
	return len(texts) == 0 || len(texts) == 1 && emptyStr(texts[0])
}

// chatPart is a chat content part: a text part joins d.texts[t0:t1]; an
// image part has the data URL "data:"+a+";base64,"+b (base64) or the URL a.
type chatPart struct {
	image, base64 bool
	t0, t1        int
	a, b          []byte // string literals
}

// userMessage splits a user message 1:N: one tool message per tool_result,
// then one user message with the remaining text and images.
func (d *decoder) userMessage(raw []byte, where loc) error {
	blocks, err := d.parseContent(raw, where, &d.blocks)
	if err != nil {
		return err
	}
	d.texts, d.parts = d.texts[:0], d.parts[:0]
	var trailing []chatPart
	nTool := 0
	for i := range blocks {
		b := &blocks[i]
		d.noteCache(b.cacheControl)
		switch b.typ {
		case "text":
			d.appendText(b.text)
		case "image":
			p, err := imagePart(b, where)
			if err != nil {
				return err
			}
			d.parts = append(d.parts, p)
		case "tool_result":
			if b.toolUseID == "" {
				return errorf("%s: tool_result.tool_use_id is required", where)
			}
			imgs, err := d.toolResult(b, where, trailing)
			if err != nil {
				return err
			}
			trailing = imgs
			nTool++
		case "thinking", "redacted_thinking":
			d.downgrade("thinking")
		default:
			return errorf("%s: unsupported content block type %q in user message", where, b.typ)
		}
	}
	d.parts = append(d.parts, trailing...)
	if len(d.parts) > 0 || nTool == 0 {
		d.beginMsg("user")
		d.appendParts()
		d.out = append(d.out, '}')
	}
	return nil
}

// appendText adds a text, joining it to a directly preceding text part.
func (d *decoder) appendText(text []byte) {
	d.texts = append(d.texts, text)
	if n := len(d.parts); n > 0 && !d.parts[n-1].image {
		d.parts[n-1].t1 = len(d.texts)
		return
	}
	d.parts = append(d.parts, chatPart{t0: len(d.texts) - 1, t1: len(d.texts)})
}

// appendParts writes d.parts as message content: a plain string when there
// are no images.
func (d *decoder) appendParts() {
	parts := d.parts
	switch {
	case len(parts) == 0:
		d.out = append(d.out, `""`...)
		return
	case len(parts) == 1 && !parts[0].image:
		d.out = jsonscan.AppendJoined(d.out, d.texts[parts[0].t0:parts[0].t1], "\n\n")
		return
	}
	d.out = append(d.out, '[')
	for k, p := range parts {
		if k > 0 {
			d.out = append(d.out, ',')
		}
		if !p.image {
			d.out = append(d.out, `{"type":"text","text":`...)
			d.out = jsonscan.AppendJoined(d.out, d.texts[p.t0:p.t1], "\n\n")
			d.out = append(d.out, '}')
			continue
		}
		d.out = append(d.out, `{"type":"image_url","image_url":{"url":`...)
		if p.base64 {
			d.out = append(d.out, `"data:`...)
			d.out = jsonscan.AppendStrBody(d.out, p.a)
			d.out = append(d.out, ";base64,"...)
			d.out = jsonscan.AppendStrBody(d.out, p.b)
			d.out = append(d.out, '"')
		} else {
			d.out = jsonscan.AppendStrLit(d.out, p.a)
		}
		d.out = append(d.out, "}}"...)
	}
	d.out = append(d.out, ']')
}

func imagePart(b *inBlock, where loc) (chatPart, error) {
	if !b.hasSource {
		return chatPart{}, errorf("%s: image.source is required", where)
	}
	src := &b.source
	switch src.typ {
	case "base64":
		if emptyStr(src.mediaType) || emptyStr(src.data) {
			return chatPart{}, errorf("%s: base64 image requires media_type and data", where)
		}
		return chatPart{image: true, base64: true, a: src.mediaType, b: src.data}, nil
	case "url":
		if emptyStr(src.url) {
			return chatPart{}, errorf("%s: url image requires url", where)
		}
		return chatPart{image: true, a: src.url}, nil
	}
	return chatPart{}, errorf("%s: unsupported image source type %q", where, src.typ)
}

// toolResult writes a tool message for a tool_result block: its content
// flattened into text; its images are appended to imgs.
func (d *decoder) toolResult(b *inBlock, where loc, imgs []chatPart) ([]chatPart, error) {
	var texts [][]byte
	if !absentOrNull(b.content) {
		blocks, err := d.parseContent(b.content, where, &d.nested) // blocks is still in use
		if err != nil {
			return nil, err
		}
		for i := range blocks {
			c := &blocks[i]
			d.noteCache(c.cacheControl)
			switch c.typ {
			case "text":
				texts = append(texts, c.text)
			case "image":
				p, err := imagePart(c, where)
				if err != nil {
					return nil, err
				}
				imgs = append(imgs, p)
			default:
				return nil, errorf("%s: unsupported content block type %q in tool_result", where, c.typ)
			}
		}
	}
	d.beginMsg("tool")
	if b.isError {
		d.out = append(d.out, `"Error: `...)
		for k, t := range texts {
			if k > 0 {
				d.out = append(d.out, '\\', 'n')
			}
			d.out = jsonscan.AppendStrBody(d.out, t)
		}
		d.out = append(d.out, '"')
	} else {
		d.out = jsonscan.AppendJoined(d.out, texts, "\n")
	}
	d.out = append(d.out, `,"tool_call_id":`...)
	d.out = jsonscan.AppendQuoted(d.out, b.toolUseID)
	d.out = append(d.out, '}')
	return imgs, nil
}

// assistantMessage maps text and tool_use blocks to one assistant message.
func (d *decoder) assistantMessage(raw []byte, where loc) error {
	blocks, err := d.parseContent(raw, where, &d.blocks)
	if err != nil {
		return err
	}
	texts := d.texts[:0]
	calls := 0
	for i := range blocks {
		b := &blocks[i]
		d.noteCache(b.cacheControl)
		switch b.typ {
		case "text":
			texts = append(texts, b.text)
		case "tool_use":
			if b.id == "" || b.name == "" {
				return errorf("%s: tool_use requires id and name", where)
			}
			if !absentOrNull(b.input) && b.input[0] != '{' {
				return errorf("%s: tool_use.input must be an object", where)
			}
			calls++
		case "thinking", "redacted_thinking":
			d.downgrade("thinking")
		default:
			return errorf("%s: unsupported content block type %q in assistant message", where, b.typ)
		}
	}
	d.texts = texts

	d.beginMsg("assistant")
	switch {
	case len(texts) > 0:
		d.out = jsonscan.AppendJoined(d.out, texts, "\n\n")
	case calls == 0:
		d.out = append(d.out, `""`...)
	default:
		d.out = append(d.out, "null"...)
	}
	if calls > 0 {
		d.out = append(d.out, `,"tool_calls":[`...)
		n := 0
		for i := range blocks {
			b := &blocks[i]
			if b.typ != "tool_use" {
				continue
			}
			if n > 0 {
				d.out = append(d.out, ',')
			}
			n++
			d.out = append(d.out, `{"id":`...)
			d.out = jsonscan.AppendQuoted(d.out, b.id)
			d.out = append(d.out, `,"type":"function","function":{"name":`...)
			d.out = jsonscan.AppendQuoted(d.out, b.name)
			d.out = append(d.out, `,"arguments":`...)
			if absentOrNull(b.input) {
				d.out = append(d.out, `"{}"`...)
			} else {
				d.out = jsonscan.AppendQuoted(d.out, string(d.x.AppendCompact(nil, b.input)))
			}
			d.out = append(d.out, "}}"...)
		}
		d.out = append(d.out, ']')
	}
	d.out = append(d.out, '}')
	return nil
}

// tools keeps custom tools and drops Anthropic built-in tools. The chat
// tools are appended to d.out as the "tools" member (omitted if there are
// none); it returns their number.
func (d *decoder) tools(raw []byte) (int, error) {
	list, ok := jsonscan.DecodeSlice[inTool](d.x, nil, raw, true, decodeTool)
	if !ok {
		return 0, errorf("tools: must be an array of tool definitions")
	}
	mark := len(d.out)
	out := append(d.out, `,"tools":[`...)
	n := 0
	for i := range list {
		t := &list[i]
		d.noteCache(t.cacheControl)
		if t.typ != "" && t.typ != "custom" {
			d.downgrade("tool:" + t.typ)
			continue
		}
		if t.strict {
			d.downgrade("tools.strict")
		}
		if t.name == "" {
			return 0, errorf("tools[%d]: name is required", i)
		}
		if absentOrNull(t.inputSchema) {
			return 0, errorf("tools[%d]: input_schema is required", i)
		}
		if n > 0 {
			out = append(out, ',')
		}
		n++
		out = append(out, `{"type":"function","function":{"name":`...)
		out = jsonscan.AppendQuoted(out, t.name)
		if !emptyStr(t.desc) {
			out = append(out, `,"description":`...)
			out = jsonscan.AppendStrLit(out, t.desc)
		}
		out = append(out, `,"parameters":`...)
		out = d.x.AppendCompact(out, t.inputSchema)
		out = append(out, "}}"...)
	}
	if n == 0 {
		out = out[:mark]
	} else {
		out = append(out, ']')
	}
	d.out = out
	return n, nil
}

// toolChoice maps an Anthropic tool_choice; noParallel reports
// disable_parallel_tool_use.
func toolChoice(raw json.RawMessage) (choice any, noParallel bool, err error) {
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, false, errorf("tool_choice: must be an object")
	}
	switch tc.Type {
	case "auto":
		choice = "auto"
	case "any":
		choice = "required"
	case "none":
		choice = "none"
	case "tool":
		if tc.Name == "" {
			return nil, false, errorf("tool_choice: name is required for type \"tool\"")
		}
		choice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
	default:
		return nil, false, errorf("tool_choice: unsupported type %q", tc.Type)
	}
	return choice, tc.DisableParallelToolUse, nil
}
