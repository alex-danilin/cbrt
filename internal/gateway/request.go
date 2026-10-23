package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/alex-danilin/cbrt/internal/api/anthropic"
	"github.com/alex-danilin/cbrt/internal/config"
	"github.com/alex-danilin/cbrt/internal/jsonspan"
)

// chatRequest is a Chat Completions request kept as the raw byte spans of its
// top-level members, so unknown fields pass through byte-for-byte, plus what
// routing needs to know about it. The body is scanned once (jsonspan validates
// while splitting); per-target upstream bodies are assembled by splicing spans.
type chatRequest struct {
	body     []byte
	members  []jsonspan.Member
	messages []message // spans of the "messages" elements

	Model              string
	Stream             bool
	ClientWantsUsage   bool     // client itself asked for stream_options.include_usage
	Needs              []string // capabilities: tools, vision, json
	ToolChoiceRequired bool
	EstPromptTokens    int
	MaxOut             int    // requested output tokens (0 = unspecified)
	tokenField         string // which field carried MaxOut

	// Raw is the client's original Anthropic Messages body, for passthrough
	// targets; nil for other client APIs.
	Raw []byte
	// untranslatable is why Raw could not be translated for other targets;
	// such a request can only be relayed unchanged.
	untranslatable error

	normOnce sync.Once // normalized messages are the same for every target that normalizes
	normMsgs []byte
	normed   bool
}

// message is one element of "messages": its raw bytes, members and role.
type message struct {
	raw     []byte
	members []jsonspan.Member
	role    string
	content []byte
}

var errBadRequest = errors.New("invalid request")

// parseChat validates the minimum the gateway relies on; everything else is the upstream's business.
func parseChat(body []byte) (*chatRequest, error) {
	members, err := jsonspan.Object(body)
	if err != nil {
		return nil, fmt.Errorf("%w: body is not a JSON object: %v", errBadRequest, err)
	}
	r := &chatRequest{body: body, members: members}

	if raw := r.field("model"); raw == nil || json.Unmarshal(raw, &r.Model) != nil || r.Model == "" {
		return nil, fmt.Errorf("%w: \"model\" must be a non-empty string", errBadRequest)
	}
	raw := r.field("messages")
	var els [][]byte
	if raw != nil {
		els, err = jsonspan.Array(raw)
	}
	if raw == nil || err != nil || len(els) == 0 {
		return nil, fmt.Errorf("%w: \"messages\" must be a non-empty array", errBadRequest)
	}
	r.messages = make([]message, len(els))
	for i, el := range els {
		if el[0] != '{' && string(el) != "null" {
			return nil, fmt.Errorf("%w: \"messages\" must contain objects", errBadRequest)
		}
		m := message{raw: el}
		if el[0] == '{' {
			m.members, _ = jsonspan.Object(el) // already validated by Array
			if rr := jsonspan.Get(m.members, "role"); rr != nil {
				json.Unmarshal(rr, &m.role)
			}
			m.content = jsonspan.Get(m.members, "content")
		}
		r.messages[i] = m
	}

	if raw := r.field("stream"); raw != nil {
		if err := json.Unmarshal(raw, &r.Stream); err != nil {
			return nil, fmt.Errorf("%w: \"stream\" must be a boolean", errBadRequest)
		}
	}
	if raw := r.field("stream_options"); raw != nil && raw[0] == '{' {
		so, _ := jsonspan.Object(raw)
		r.ClientWantsUsage = string(jsonspan.Get(so, "include_usage")) == "true"
	}
	if raw := r.field("tools"); raw != nil && string(raw) != "null" && !isEmptyArray(raw) {
		r.Needs = append(r.Needs, "tools")
	}
	if raw := r.field("tool_choice"); raw != nil && string(raw) == `"required"` {
		r.ToolChoiceRequired = true
	}
	images, imageBytes := 0, 0
	for _, m := range r.messages {
		n, b := imageParts(m.content)
		images, imageBytes = images+n, imageBytes+b
	}
	if images > 0 {
		r.Needs = append(r.Needs, "vision")
	}
	// Text is ~4 bytes a token; an image costs a fixed estimate, not its base64 size.
	r.EstPromptTokens = estimateTokens(len(body)-imageBytes) + images*imageTokenEstimate
	if raw := r.field("response_format"); raw != nil && raw[0] == '{' {
		rf, _ := jsonspan.Object(raw)
		if ty := string(jsonspan.Get(rf, "type")); ty == `"json_object"` || ty == `"json_schema"` {
			r.Needs = append(r.Needs, "json")
		}
	}
	for _, f := range []string{"max_completion_tokens", "max_tokens"} {
		if raw := r.field(f); raw != nil && json.Unmarshal(raw, &r.MaxOut) == nil {
			r.tokenField = f
			break
		}
	}
	return r, nil
}

// field returns the raw value of a top-level member (last one wins, as in encoding/json).
func (r *chatRequest) field(name string) []byte { return jsonspan.Get(r.members, name) }

func isEmptyArray(raw []byte) bool {
	els, err := jsonspan.Array(raw)
	return err == nil && len(els) == 0
}

// imageParts counts the image parts of message content (an array of parts) and
// the bytes of their inline data: URL literals, which are image payload rather
// than text. Plain string content, and arrays without the word "image", are
// skipped without parsing them.
func imageParts(content []byte) (n, dataBytes int) {
	if len(content) == 0 || content[0] != '[' || !bytes.Contains(content, []byte(`image`)) {
		return 0, 0
	}
	parts, err := jsonspan.Array(content)
	if err != nil {
		return 0, 0
	}
	for _, p := range parts {
		if p[0] != '{' {
			continue
		}
		ms, _ := jsonspan.Object(p)
		switch string(jsonspan.Get(ms, "type")) {
		case `"image_url"`, `"input_image"`, `"image"`:
			n++
			dataBytes += len(dataURL(jsonspan.Get(ms, "image_url")))
		}
	}
	return n, dataBytes
}

// dataURL returns the data: URL string literal of an image_url value, which is
// either the URL string (input_image) or an object with a "url" member.
func dataURL(v []byte) []byte {
	if len(v) > 0 && v[0] == '{' {
		ms, _ := jsonspan.Object(v)
		v = jsonspan.Get(ms, "url")
	}
	if bytes.HasPrefix(v, []byte(`"data:`)) {
		return v
	}
	return nil
}

// imageTokenEstimate is the prompt-token estimate per image part, used instead
// of the size of its (base64) payload; shared with count_tokens.
const imageTokenEstimate = anthropic.ImageTokens

// minOutputTokens is the output allowance a target's context must leave after
// the estimated prompt (or less, if the client asked for less).
const minOutputTokens = 256

// estimateTokens is a deliberately rough chars/4 estimate, used for context
// filtering and when an upstream reports no usage.
func estimateTokens(chars int) int { return (chars + 3) / 4 }

// upstreamBody encodes the request for one target: model rewritten, the target's
// token-limit field, usage requested on streams, output clamped to fit the context,
// system messages normalized. Every other member is copied verbatim.
func (r *chatRequest) upstreamBody(t config.Target, maxContext int) ([]byte, error) {
	over := map[string][]byte{"model": quote(t.Model)}
	drop := map[string]bool{}

	if t.MidSystem != "keep" {
		if msgs, changed := r.normalized(); changed {
			over["messages"] = msgs
		}
	}
	if r.Stream && t.WantsStreamUsage() {
		over["stream_options"] = withIncludeUsage(r.field("stream_options"))
	}
	maxOut, field := r.MaxOut, r.tokenField
	if maxContext > 0 && maxOut > 0 && r.EstPromptTokens+maxOut > maxContext {
		// clamp to what the context leaves; skipReason keeps that >= minOutputTokens
		maxOut = max(1, maxContext-r.EstPromptTokens)
	}
	if t.TokenParam != "" && field != "" && field != t.TokenParam {
		drop[field] = true
		field = t.TokenParam
	}
	if field != "" && (maxOut != r.MaxOut || field != r.tokenField) {
		over[field] = strconv.AppendInt(nil, int64(maxOut), 10)
	}

	var b bytes.Buffer
	b.Grow(len(r.body) + 64)
	b.WriteByte('{')
	written := map[string]bool{}
	sep := func() {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
	}
	for _, m := range r.members {
		if drop[m.Key] {
			continue
		}
		if v, ok := over[m.Key]; ok {
			if written[m.Key] {
				continue // duplicate key: the override replaces all occurrences
			}
			written[m.Key] = true
			sep()
			b.Write(m.KeyRaw)
			b.WriteByte(':')
			b.Write(v)
			continue
		}
		sep()
		b.Write(m.KeyRaw)
		b.WriteByte(':')
		b.Write(m.Value)
	}
	for _, k := range []string{"model", "messages", "stream_options", "max_tokens", "max_completion_tokens"} {
		if v, ok := over[k]; ok && !written[k] {
			sep()
			b.Write(quote(k))
			b.WriteByte(':')
			b.Write(v)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// withIncludeUsage returns stream_options with include_usage set to true, keeping other options.
func withIncludeUsage(raw []byte) []byte {
	ms, err := jsonspan.Object(raw)
	if raw == nil || err != nil {
		return []byte(`{"include_usage":true}`)
	}
	var b bytes.Buffer
	b.WriteString(`{"include_usage":true`)
	for _, m := range ms {
		if m.Key == "include_usage" {
			continue
		}
		b.WriteByte(',')
		b.Write(m.KeyRaw)
		b.WriteByte(':')
		b.Write(m.Value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func quote(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// normalized computes the system-normalized messages once per request.
func (r *chatRequest) normalized() ([]byte, bool) {
	r.normOnce.Do(func() { r.normMsgs, r.normed = normalizeMessages(r.messages) })
	return r.normMsgs, r.normed
}

// normalizeSystem is normalizeMessages for a raw "messages" array (used by tests).
func normalizeSystem(raw json.RawMessage) (json.RawMessage, bool) {
	els, err := jsonspan.Array(raw)
	if err != nil {
		return raw, false
	}
	msgs := make([]message, len(els))
	for i, el := range els {
		msgs[i] = message{raw: el}
		if el[0] == '{' {
			msgs[i].members, _ = jsonspan.Object(el)
			json.Unmarshal(jsonspan.Get(msgs[i].members, "role"), &msgs[i].role)
			msgs[i].content = jsonspan.Get(msgs[i].members, "content")
		}
	}
	out, changed := normalizeMessages(msgs)
	if !changed {
		return raw, false
	}
	return out, true
}

// normalizeMessages rewrites system messages that are not at the start of the
// conversation, which many open-model chat templates reject ("System message
// must be at the beginning"). Claude Code sends them every turn. Rule:
// system messages before the first assistant turn are merged into one leading
// system message; later ones become <system-reminder> text in a user message.
// Several leading system messages (Codex: instructions + developer) are joined
// too: strict templates (Gemma) accept only one, at position 0.
func normalizeMessages(msgs []message) ([]byte, bool) {
	leading := 0
	for leading < len(msgs) && msgs[leading].role == "system" {
		leading++
	}
	late := false
	for _, m := range msgs[leading:] {
		if m.role == "system" {
			late = true
			break
		}
	}
	if !late && leading < 2 {
		return nil, false
	}

	var sys []string
	for _, m := range msgs[:leading] {
		sys = append(sys, contentText(m.content))
	}
	// out holds raw message objects; pending is a user message whose string
	// content may still receive reminders.
	type outMsg struct {
		raw        []byte
		members    []jsonspan.Member
		textUser   bool   // a user message with string content
		text       string // its content, when textUser
		reminders  []string
		standalone bool // a new user message made only of reminders
	}
	var out []outMsg
	seenAssistant := false
	for _, m := range msgs[leading:] {
		switch m.role {
		case "assistant":
			seenAssistant = true
		case "system":
			text := contentText(m.content)
			if !seenAssistant {
				sys = append(sys, text)
				continue
			}
			reminder := "<system-reminder>\n" + text + "\n</system-reminder>"
			if n := len(out); n > 0 && (out[n-1].textUser || out[n-1].standalone) {
				out[n-1].reminders = append(out[n-1].reminders, reminder)
				continue
			}
			out = append(out, outMsg{standalone: true, reminders: []string{reminder}})
			continue
		}
		om := outMsg{raw: m.raw, members: m.members}
		if m.role == "user" && len(m.content) > 0 && m.content[0] == '"' {
			if json.Unmarshal(m.content, &om.text) == nil {
				om.textUser = true
			}
		}
		out = append(out, om)
	}

	var b bytes.Buffer
	b.WriteByte('[')
	first := true
	sep := func() {
		if !first {
			b.WriteByte(',')
		}
		first = false
	}
	if len(sys) > 0 {
		sep()
		b.WriteString(`{"role":"system","content":`)
		b.Write(quote(strings.Join(sys, "\n\n")))
		b.WriteByte('}')
	}
	for _, om := range out {
		sep()
		switch {
		case om.standalone:
			b.WriteString(`{"role":"user","content":`)
			b.Write(quote(strings.Join(om.reminders, "\n\n")))
			b.WriteByte('}')
		case len(om.reminders) > 0:
			// user message with appended reminders: rewrite only its content
			content := quote(strings.Join(append([]string{om.text}, om.reminders...), "\n\n"))
			b.WriteByte('{')
			wrote := false
			for i, mm := range om.members {
				if mm.Key == "content" && wrote {
					continue
				}
				if i > 0 {
					b.WriteByte(',')
				}
				b.Write(mm.KeyRaw)
				b.WriteByte(':')
				if mm.Key == "content" {
					b.Write(content)
					wrote = true
				} else {
					b.Write(mm.Value)
				}
			}
			b.WriteByte('}')
		default:
			b.Write(om.raw)
		}
	}
	b.WriteByte(']')
	return b.Bytes(), true
}

// contentText flattens a string or an array of text parts.
func contentText(raw []byte) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &parts)
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// skipReason explains why a target cannot serve this request, or returns "".
func (r *chatRequest) skipReason(t config.Target, maxContext int) string {
	for _, n := range r.Needs {
		if !t.HasCapability(n) {
			return "no_" + n
		}
	}
	if r.ToolChoiceRequired && !t.SupportsToolChoiceRequired() {
		return "no_tool_choice_required"
	}
	if maxContext > 0 {
		// any answer needs room: the minimum allowance, or the client's smaller cap
		need := minOutputTokens
		if r.MaxOut > 0 && r.MaxOut < need {
			need = r.MaxOut
		}
		if maxContext-r.EstPromptTokens < need {
			return "context_too_small"
		}
	}
	return ""
}
