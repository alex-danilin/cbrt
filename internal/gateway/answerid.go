package gateway

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// answerID returns the id of the answer the client received, from one JSON
// object the gateway wrote: a whole response, or an SSE event's data. It is
// recorded with the attempt so a client's transcript (which stores this id)
// can be matched to what cbrt served. Shapes:
//   - Chat Completions response or chunk, Anthropic message: top-level "id";
//   - Anthropic message_start: message.id;
//   - Responses response.created (and other response.* events): response.id.
//
// Events without an id (pings, deltas) give "".
func answerID(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !bytes.Contains(data, []byte(`"id"`)) {
		return ""
	}
	var v struct {
		ID      string `json:"id"`
		Message *struct {
			ID string `json:"id"`
		} `json:"message"`
		Response *struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	switch {
	case v.Message != nil && v.Message.ID != "":
		return v.Message.ID
	case v.Response != nil && v.Response.ID != "":
		return v.Response.ID
	}
	return v.ID
}

// leadingIDRE finds the first "id" member of a JSON body whose start is all
// the gateway kept (a relayed non-stream answer; Anthropic puts "id" first).
var leadingIDRE = regexp.MustCompile(`^\s*\{\s*"id"\s*:\s*"([^"\\]{1,200})"`)

func leadingID(head []byte) string {
	if m := leadingIDRE.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}
