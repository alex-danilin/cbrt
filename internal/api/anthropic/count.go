package anthropic

import (
	"encoding/json"

	"github.com/alex-danilin/cbrt/internal/jsonscan"
)

// ImageTokens is the fixed per-image token estimate. CountTokens and the
// gateway's context filtering both use it, so they agree on image cost.
const ImageTokens = 1500

// countRequest is what CountTokens reads of a request, as decoded with
// encoding/json semantics by decodeCountRequest.
type countRequest struct {
	system   []byte // any JSON value or nil
	messages []message
	tools    []countTool
}

type countTool struct {
	name, desc  []byte // string literals or nil
	inputSchema []byte // any JSON value or nil
}

// CountTokens estimates the input tokens of an Anthropic Messages request
// (for POST /v1/messages/count_tokens) as about 4 bytes per token over the
// system prompt, tool definitions and message contents, plus a fixed
// estimate per image.
func CountTokens(body []byte) (int, error) {
	x := jsonscan.NewIndex(body)
	var req countRequest
	if x == nil || !decodeCountRequest(x, &req, jsonscan.TrimSpace(body)) {
		return 0, errorf("invalid request body: %v", countTokensError(body))
	}
	c := counter{x: x}
	c.content(req.system)
	for _, t := range req.tools {
		c.bytes += strLenOrZero(t.name) + strLenOrZero(t.desc) + x.CompactLen(t.inputSchema)
	}
	for _, m := range req.messages {
		c.bytes += 16 // role and message framing
		c.content(m.content)
	}
	return estimateTokens(c.bytes) + c.images*ImageTokens, nil
}

// countTokensError returns the error encoding/json reports for a body that
// decodeCountRequest rejects.
func countTokensError(body []byte) error {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []inMessage     `json:"messages"`
		Tools    []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	return json.Unmarshal(body, &req)
}

// decodeCountRequest decodes the body v like json.Unmarshal into the struct
// in countTokensError.
func decodeCountRequest(x *jsonscan.Index, req *countRequest, v []byte) bool {
	switch v[0] {
	case 'n':
		return true
	case '{':
	default:
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
		case "system":
			req.system = val
		case "messages":
			if jsonscan.IsNull(val) {
				req.messages = nil
			} else {
				req.messages, ok = jsonscan.DecodeSlice(x, req.messages, val, false, decodeMessage)
			}
		case "tools":
			if jsonscan.IsNull(val) {
				req.tools = nil
			} else {
				req.tools, ok = jsonscan.DecodeSlice(x, req.tools, val, false, decodeCountTool)
			}
		}
		if !ok {
			return false
		}
	}
}

func decodeCountTool(x *jsonscan.Index, t *countTool, v []byte) bool {
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
		case "name":
			ok = jsonscan.FieldRawStr(val, &t.name)
		case "description":
			ok = jsonscan.FieldRawStr(val, &t.desc)
		case "input_schema":
			t.inputSchema = val
		}
		if !ok {
			return false
		}
	}
}

func strLenOrZero(s []byte) int {
	if s == nil {
		return 0
	}
	return jsonscan.StrLen(s)
}

type counter struct {
	x             *jsonscan.Index
	bytes, images int
	scratch       []countBlock
}

// countBlock is what the counter reads of a content block.
type countBlock struct {
	typ                  string
	text, thinking, name []byte // string literals or nil
	input, content       []byte // any JSON value or nil
}

func decodeCountBlock(x *jsonscan.Index, b *countBlock, v []byte) bool {
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
		case "thinking":
			ok = jsonscan.FieldRawStr(val, &b.thinking)
		case "name":
			ok = jsonscan.FieldRawStr(val, &b.name)
		case "input":
			b.input = val
		case "content":
			b.content = val
		}
		if !ok {
			return false
		}
	}
}

// content counts a string or an array of blocks; unknown shapes (anything
// that does not decode into the blocks) are ignored.
func (c *counter) content(raw []byte) {
	if absentOrNull(raw) {
		return
	}
	if raw[0] == '"' {
		c.bytes += jsonscan.StrLen(raw)
		return
	}
	blocks, ok := jsonscan.DecodeSlice(c.x, c.scratch, raw, true, decodeCountBlock)
	if !ok {
		return
	}
	// Nested content is counted with fresh scratch space.
	c.scratch = nil
	for i := range blocks {
		b := &blocks[i]
		switch b.typ {
		case "image":
			c.images++
		case "thinking", "redacted_thinking":
			// Dropped before the request goes upstream.
		default:
			c.bytes += strLenOrZero(b.text) + strLenOrZero(b.name)
			if !absentOrNull(b.input) {
				c.bytes += c.x.CompactLen(b.input)
			}
			c.content(b.content)
		}
	}
	c.scratch = blocks
}
