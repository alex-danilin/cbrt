package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// maxToolName is the longest function name most Chat Completions upstreams
// accept.
const maxToolName = 64

var validToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ToolMap records how the Responses tools of a request were flattened into
// Chat Completions function tools, so that calls can be mapped back: a
// flattened namespace tool returns to {namespace, name} and a custom tool
// returns as a custom_tool_call. The zero value maps every name to a plain
// function tool of the same name.
type ToolMap struct {
	byChat map[string]toolRef // chat function name -> original tool
	byOrig map[toolKey]string // original tool -> chat function name
}

type toolKey struct{ namespace, name string }

type toolRef struct {
	namespace string
	name      string
	custom    bool
}

// lookup maps a chat function name back to the Responses tool.
func (m ToolMap) lookup(chatName string) toolRef {
	if r, ok := m.byChat[chatName]; ok {
		return r
	}
	return toolRef{name: chatName}
}

// chatName maps a Responses tool (as echoed in history items) to the chat
// function name. Tools not in the map are flattened the same way.
func (m ToolMap) chatName(namespace, name string) string {
	if n, ok := m.byOrig[toolKey{namespace, name}]; ok {
		return n
	}
	if namespace == "" {
		return name
	}
	return flatName(namespace, name, false)
}

// add registers a tool and returns its chat name, or "" if the name
// collides with a different tool even after hashing.
func (m *ToolMap) add(ref toolRef) string {
	if m.byChat == nil {
		m.byChat = map[string]toolRef{}
		m.byOrig = map[toolKey]string{}
	}
	key := toolKey{ref.namespace, ref.name}
	if _, dup := m.byOrig[key]; dup {
		return ""
	}
	// Candidates: the plain name, or the flattened name and then its hashed form.
	for try := 0; try < 2; try++ {
		var n string
		switch {
		case ref.namespace == "":
			if try > 0 {
				return ""
			}
			n = ref.name
		default:
			n = flatName(ref.namespace, ref.name, try > 0)
		}
		if old, ok := m.byChat[n]; ok && (old.namespace != ref.namespace || old.name != ref.name) {
			continue
		}
		m.byChat[n] = ref
		m.byOrig[key] = n
		return n
	}
	return ""
}

// flatName returns "<namespace>__<name>". Names that are too long or contain
// characters upstreams reject (or when forceHash is set) are sanitized and
// shortened deterministically with a hash suffix.
func flatName(namespace, name string, forceHash bool) string {
	n := namespace + "__" + name
	if !forceHash && len(n) <= maxToolName && isToolName(n) {
		return n
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	suffix := "_" + hex.EncodeToString(sum[:4])
	b := make([]byte, 0, len(n)+len(suffix))
	for _, r := range n {
		if r < 128 && isToolNameByte(byte(r)) {
			b = append(b, byte(r))
		} else {
			b = append(b, '_')
		}
	}
	if len(b) > maxToolName-len(suffix) {
		b = b[:maxToolName-len(suffix)]
	}
	return string(append(b, suffix...))
}

// isToolName is validToolName.MatchString without the regexp.
func isToolName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isToolNameByte(s[i]) {
			return false
		}
	}
	return true
}

func isToolNameByte(c byte) bool {
	return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
