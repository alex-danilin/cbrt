// Command synth-upstream writes every file under testdata/upstream/.
//
// The fixtures are SYNTHETIC: all ids, timestamps, texts, reasoning, tool
// arguments, token counts and error messages are invented here. Only the
// structure (field names and order, nesting, chunk layout, event order,
// finish reasons, where reasoning, tool calls and usage appear) is modelled
// on responses from NVIDIA build (deepseek, gpt-oss) and Ollama's
// OpenAI-compatible API (/v1/chat/completions and /v1/responses).
//
// Run from the repository root; the output is deterministic:
//
//	go run ./scripts/synth-upstream
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// base is the invented creation time used by every fixture (2026-01-01 UTC).
const base = 1767225600

var files = map[string]string{}

func main() {
	nvidia()
	ollama()
	root := filepath.Join("testdata", "upstream")
	if st, err := os.Stat("testdata"); err != nil || !st.IsDir() {
		fmt.Fprintln(os.Stderr, "synth-upstream: run from the repository root")
		os.Exit(1)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(root, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(p, []byte(files[n]), 0o644); err != nil {
			fail(err)
		}
	}
	fmt.Printf("synth-upstream: wrote %d files under %s\n", len(names), root)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "synth-upstream:", err)
	os.Exit(1)
}

// q returns s as a JSON string literal without HTML escaping.
func q(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		fail(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// tokens splits s into stream deltas the way a tokenizer roughly would:
// words keep their leading space, punctuation and newlines form their own
// pieces.
func tokens(s string) []string {
	const (
		word = iota
		space
		newline
		punct
	)
	class := func(r rune) int {
		switch {
		case r == '\n':
			return newline
		case r == ' ':
			return space
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			return word
		}
		return punct
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	prev := -1
	for _, r := range s {
		c := class(r)
		switch {
		case c == space:
			flush()
		case c == word && (prev == word || prev == space):
		case c == punct && prev == punct:
		case c == newline && prev == newline:
		default:
			flush()
		}
		cur.WriteRune(r)
		prev = c
	}
	flush()
	return out
}

// sse renders data payloads as an OpenAI-style event stream.
func sse(chunks []string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func args(city, sep string) string {
	return `{"city":` + sep + q(city) + `}`
}

// ---------------------------------------------------------------- NVIDIA

const dsModel = "deepseek-ai/deepseek-v4.1-flash"

func dsChunk(id string, created int, delta string) string {
	return fmt.Sprintf(`{"id":%s,"choices":[{"index":0,"delta":%s,"finish_reason":null,"logprobs":null}],"created":%d,"model":%s,"object":"chat.completion.chunk"}`,
		q(id), delta, created, q(dsModel))
}

func dsFinal(id string, created int, finish, usage string) string {
	return fmt.Sprintf(`{"id":%s,"choices":[{"index":0,"delta":{},"finish_reason":%s,"logprobs":null}],"created":%d,"model":%s,"object":"chat.completion.chunk","usage":%s}`,
		q(id), q(finish), created, q(dsModel), usage)
}

func dsUsage(prompt, completion, cached, reasoning int) string {
	return fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens_details":{"reasoning_tokens":%d}}`,
		prompt, completion, prompt+completion, cached, reasoning)
}

func dsToolCall(id, city string) string {
	return fmt.Sprintf(`{"id":%s,"type":"function","function":{"name":"get_weather","arguments":%s}}`, q(id), q(args(city, " ")))
}

func gptToolCall(id, city string) string {
	return dsToolCall(id, city)
}

func nvidia() {
	// Non-stream: reasoning, a short text, then two parallel tool calls.
	files["nvidia/deepseek-v4.1-flash_parallel-tools.json"] = fmt.Sprintf(
		`{"id":"synth0ds00000001","choices":[{"index":0,"message":{"content":%s,"refusal":null,"tool_calls":[%s,%s],"role":"assistant","reasoning_content":%s},"finish_reason":"tool_calls","logprobs":null}],"created":%d,"model":%s,"object":"chat.completion","usage":%s}`,
		q("Looking up both forecasts at once.\n\n"),
		dsToolCall("chatcmpl-tool-synth000000000a1", "Oslo"),
		dsToolCall("chatcmpl-tool-synth000000000a2", "Lima"),
		q("Two independent lookups, so both get_weather calls can go out together."),
		base+101, q(dsModel), dsUsage(320, 88, 256, 17))

	// Non-stream: tool_choice "required" but the model only reasoned.
	files["nvidia/deepseek-v4.1-flash_tool-choice-required.json"] = fmt.Sprintf(
		`{"id":"synth0ds00000002","choices":[{"index":0,"message":{"content":null,"refusal":null,"role":"assistant","reasoning_content":%s},"finish_reason":"stop","logprobs":null}],"created":%d,"model":%s,"object":"chat.completion","usage":%s}`,
		q("The ping tool has no parameters, so an empty argument object would do."),
		base+102, q(dsModel), dsUsage(250, 20, 128, 18))

	// Stream: reasoning_content deltas, then text, usage in the finish chunk.
	{
		id, c := "synth0ds00000003", base+103
		var ch []string
		reasoning := []string{"The", " user wants", " three primary", " colors.", " Classic", " answer:", " red, yellow,", " blue.", " Keep it", " short."}
		for i, r := range reasoning {
			if i == 0 {
				ch = append(ch, dsChunk(id, c, `{"role":"assistant","reasoning_content":`+q(r)+`}`))
				continue
			}
			ch = append(ch, dsChunk(id, c+min(i/3, 1), `{"reasoning_content":`+q(r)+`}`))
		}
		for _, t := range []string{"Red, ", "yellow, ", "blue."} {
			ch = append(ch, dsChunk(id, c+1, `{"content":`+q(t)+`}`))
		}
		ch = append(ch, dsFinal(id, c+1, "stop", dsUsage(30, 44, 0, 30)))
		files["nvidia/deepseek-v4.1-flash_stream.sse"] = sse(ch)
	}

	// Stream: reasoning overlapping the first text delta, text, then two tool
	// calls whose arguments arrive in fragments.
	{
		id, c := "synth0ds00000004", base+104
		var ch []string
		ch = append(ch, dsChunk(id, c, `{"role":"assistant","reasoning_content":"Both"}`))
		for _, r := range []string{" lookups are", " independent; call", " get_weather for Oslo and Lima"} {
			ch = append(ch, dsChunk(id, c, `{"reasoning_content":`+q(r)+`}`))
		}
		ch = append(ch, dsChunk(id, c, `{"content":"Looking","reasoning_content":" together."}`))
		for _, t := range []string{" up both", " forecasts", " at once", ".\n\n"} {
			ch = append(ch, dsChunk(id, c, `{"content":`+q(t)+`}`))
		}
		call := func(idx int, callID string, frags ...string) {
			ch = append(ch, dsChunk(id, c, fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":"get_weather","arguments":""}}]}`, idx, q(callID))))
			for _, f := range frags {
				ch = append(ch, dsChunk(id, c, fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}`, idx, q(f))))
			}
		}
		call(0, "chatcmpl-tool-synth000000000b1", `{"city": "`, `Oslo"}`)
		call(1, "chatcmpl-tool-synth000000000b2", `{"city": "Li`, `ma"}`)
		ch = append(ch, dsFinal(id, c, "tool_calls", dsUsage(320, 88, 192, 15)))
		files["nvidia/deepseek-v4.1-flash_parallel-tools_stream.sse"] = sse(ch)
	}

	// gpt-oss: reasoning (in both reasoning and reasoning_content) runs out of
	// tokens before any content.
	{
		r := "The request: a greeting of exactly three words. Options: \"Good morning, everyone\" counts three. " +
			"\"Hi there\" is only two. \"Warm welcome, traveler\" also has three, though punctuation may matter. " +
			"A plain one is probably best, maybe \"Hello again, friend\" or"
		files["nvidia/gpt-oss-20b_reasoning-hits-max-tokens.json"] = fmt.Sprintf(
			`{"id":"chatcmpl-synth0gpt0000001","object":"chat.completion","created":%d,"model":"openai/gpt-oss-20b","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"annotations":null,"audio":null,"function_call":null,"tool_calls":[],"reasoning":%s,"reasoning_content":%s},"logprobs":null,"finish_reason":"length","stop_reason":null,"token_ids":null}],"service_tier":null,"system_fingerprint":null,"usage":{"prompt_tokens":64,"total_tokens":184,"completion_tokens":120,"prompt_tokens_details":null},"prompt_logprobs":null,"prompt_token_ids":null,"kv_transfer_params":null}`,
			base+201, q(r), q(r))
	}
	{
		r := "Two cities were asked about, Oslo and Lima, and the tool should be used for each. " +
			"Start with get_weather for Oslo; the second lookup can follow once this result is back."
		files["nvidia/gpt-oss-20b_tools.json"] = fmt.Sprintf(
			`{"id":"chatcmpl-synth0gpt0000002","object":"chat.completion","created":%d,"model":"openai/gpt-oss-20b","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":null,"annotations":null,"audio":null,"function_call":null,"tool_calls":[%s],"reasoning":%s,"reasoning_content":%s},"logprobs":null,"finish_reason":"tool_calls","stop_reason":200012,"token_ids":null}],"service_tier":null,"system_fingerprint":null,"usage":{"prompt_tokens":140,"total_tokens":215,"completion_tokens":75,"prompt_tokens_details":null},"prompt_logprobs":null,"prompt_token_ids":null,"kv_transfer_params":null}`,
			base+202, gptToolCall("chatcmpl-tool-synth000000000c1", "Oslo"), q(r), q(r))
	}

	// Errors. Each carries the phrase the classifier keys on.
	files["nvidia/error_403_invalid-key.json"] = `{"status":403,"title":"Forbidden","detail":"Authorization failed: key rejected"}`
	files["nvidia/error_404_not-found-for-account.json"] = `{"status":404,"title":"Not Found","detail":"Function '00000000-0000-4000-8000-0000000000aa': not available for account 'example-account'"}`
	files["nvidia/error_404_unknown-model.txt"] = "404: no route matches this path\n"
}

// ---------------------------------------------------------------- Ollama

func olChunk(id string, created int, model, choices string) string {
	return fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"system_fingerprint":"fp_ollama","choices":%s}`,
		q(id), created, q(model), choices)
}

func olDelta(delta string) string {
	return `[{"index":0,"delta":` + delta + `,"finish_reason":null}]`
}

func olFinish(finish string) string {
	return `[{"index":0,"delta":{},"finish_reason":` + q(finish) + `}]`
}

func olUsage(prompt, cached, completion int) string {
	return fmt.Sprintf(`{"prompt_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens":%d,"total_tokens":%d}`,
		prompt, cached, completion, prompt+completion)
}

// olStream renders an Ollama stream: reasoning deltas (each with an empty
// content), then content deltas, then extra chunks, the finish chunk and a
// separate usage chunk with no choices.
func olStream(id string, created int, model string, reasoning, content []string, extra []string, finish, usage string) string {
	var ch []string
	first := true
	add := func(fields string) {
		if first {
			fields = `"role":"assistant",` + fields
			first = false
		}
		ch = append(ch, olChunk(id, created, model, olDelta("{"+fields+"}")))
	}
	for _, r := range reasoning {
		add(`"content":"","reasoning":` + q(r))
	}
	for _, c := range content {
		add(`"content":` + q(c))
	}
	for _, e := range extra {
		add(e)
	}
	ch = append(ch, olChunk(id, created, model, olFinish(finish)))
	ch = append(ch, fmt.Sprintf(`{"id":%s,"object":"chat.completion.chunk","created":%d,"model":%s,"system_fingerprint":"fp_ollama","choices":[],"usage":%s}`,
		q(id), created, q(model), usage))
	return sse(ch)
}

func olMessage(id string, created int, model, message, finish, usage string) string {
	return fmt.Sprintf(`{"id":%s,"object":"chat.completion","created":%d,"model":%s,"system_fingerprint":"fp_ollama","choices":[{"index":0,"message":%s,"finish_reason":%s}],"usage":%s}`+"\n",
		q(id), created, q(model), message, q(finish), usage)
}

func olToolCall(id string, index int, city string) string {
	return fmt.Sprintf(`{"id":%s,"index":%d,"type":"function","function":{"name":"get_weather","arguments":%s}}`, q(id), index, q(args(city, "")))
}

// olError is Ollama's OpenAI-compatible error envelope.
func olError(message, typ string) string {
	return fmt.Sprintf(`{"error":{"message":%s,"type":%s,"param":null,"code":null}}`+"\n", q(message), q(typ))
}

// olContextError wraps the runner's JSON error document as a string, the
// way Ollama relays a prompt that does not fit the loaded context.
func olContextError(prompt, nCtx int) string {
	inner := fmt.Sprintf(`{"error":{"code":400,"message":%s,"type":"exceed_context_size_error","n_prompt_tokens":%d,"n_ctx":%d}}`,
		q(fmt.Sprintf("prompt holds %d tokens but the loaded context fits only %d tokens", prompt, nCtx)), prompt, nCtx)
	return olError(inner, "invalid_request_error")
}

const (
	gemma   = "gemma-4-e2b-4"
	minicpm = "minicpm-v4.6"
)

func ollama() {
	files["ollama/error_400_context-exceeded.json"] = olContextError(9000, 2048)
	files["ollama/minicpm-v4.6_responses_context-exceeded-400.json"] = olContextError(15000, 2048)
	files["ollama/error_404_unknown-model.json"] = olError("model 'missing-model' not found", "not_found_error")

	// gemma: chat completions.
	files["ollama/gemma-4-e2b-4_basic.json"] = olMessage("chatcmpl-901", base+301, gemma,
		`{"role":"assistant","content":"Good day, friend.","reasoning":`+q(
			"Plan:\n\n1.  **Goal:** Produce a three-word greeting.\n"+
				"2.  **Candidates:**\n    *   Good day, friend. (3) - fits.\n    *   Welcome back. (2) - too short.\n"+
				"    *   Nice to see you. (4) - too long.\n3.  **Pick:** The first candidate is warm and exact.")+`}`,
		"stop", olUsage(21, 0, 180))
	files["ollama/gemma-4-e2b-4_required.json"] = olMessage("chatcmpl-902", base+302, gemma,
		`{"role":"assistant","content":"Hello! What would you like to do?","reasoning":`+q(
			"Plan:\n\n1.  **Input:** A short greeting.\n2.  **Tools on offer:** only `ping`, which checks connectivity.\n"+
				"3.  **Relevance:** A greeting needs no network check.\n4.  **Reply:** Answer politely and invite a task.")+`}`,
		"stop", olUsage(34, 0, 150))
	files["ollama/gemma-4-e2b-4_tools.json"] = olMessage("chatcmpl-903", base+303, gemma,
		`{"role":"assistant","content":"","reasoning":`+q(
			"1.  **Request:** Forecasts for Oslo and Lima, using the tool for each.\n\n"+
				"2.  **Tool:** get_weather accepts one city name.\n\n"+
				"3.  **Calls:** one lookup per city, so two calls in total.")+
			`,"tool_calls":[`+olToolCall("call_synth0a1", 0, "Oslo")+`,`+olToolCall("call_synth0a2", 1, "Lima")+`]}`,
		"tool_calls", olUsage(70, 0, 200))

	// gemma: content-only stream with a separate usage chunk.
	{
		var content []string
		for i, d := range []string{"Mon", "Tue", "Wed", "Thu", "Fri"} {
			if i > 0 {
				content = append(content, ",", " ")
			}
			content = append(content, d)
		}
		files["ollama/gemma-4-e2b-4_stream.sse"] = olStream("chatcmpl-904", base+304, gemma, nil, content, nil, "stop", olUsage(20, 0, 14))
	}

	// gemma: long reasoning, then one tool call per chunk with whole arguments.
	{
		r := "1. **Read the request:** Someone wants current conditions for two places, \"Oslo\" and \"Lima\", " +
			"and asks that the tool be used for each of them.\n\n" +
			"2. **Check the tools:** There is a single tool, get_weather, which takes a city name as a string " +
			"and returns the conditions there.\n\n" +
			"3. **Work out the steps:** Each city needs its own lookup, so the tool runs twice, once with Oslo " +
			"and once with Lima. The two lookups do not depend on each other.\n\n" +
			"4. **Write the calls:**\n    *   First: get_weather with city set to Oslo\n" +
			"    *   Second: get_weather with city set to Lima\n\n" +
			"5. **Finish:** Emit both calls now; the summary for the user comes after the results return."
		extra := []string{
			`"content":"","tool_calls":[` + olToolCall("call_synth0b1", 0, "Oslo") + `]`,
			`"content":"","tool_calls":[` + olToolCall("call_synth0b2", 1, "Lima") + `]`,
		}
		files["ollama/gemma-4-e2b-4_toolsstream.sse"] = olStream("chatcmpl-905", base+305, gemma, tokens(r), nil, extra, "tool_calls", olUsage(70, 64, 190))
	}

	// minicpm: chat completions.
	files["ollama/minicpm-v4.6_basic.json"] = olMessage("chatcmpl-911", base+311, minicpm,
		`{"role":"assistant","content":"","reasoning":`+q(
			"The task is a greeting made of three words. Counting matters here, so each candidate needs a check.\n\n"+
				"\"Good morning\" is two words, which falls short. Adding a name could help, but there is no name given. "+
				"\"Good morning, all\" would be three words, and it reads naturally. Another idea is \"Hello and welcome\", "+
				"also three words, though it feels a little formal.\n\n"+
				"Should the reply include anything else? The request only asks for the greeting, so extra sentences "+
				"would break the word limit. Then again, maybe the count should ignore punctuation, which it does anyway. "+
				"Let me compare the two options once more: the first is friendlier, the second is more")+`}`,
		"length", olUsage(17, 0, 300))
	files["ollama/minicpm-v4.6_required.json"] = olMessage("chatcmpl-912", base+312, minicpm,
		`{"role":"assistant","content":"Hello! Happy to help with anything. 🙂","reasoning":`+q(
			"Alright, a short greeting arrived. The only tool here is ping, meant for checking whether a host answers, "+
				"and nothing in the message asks about a host.\n\n"+
				"The instructions favour tools when they help, but a greeting does not need one. "+
				"So the right move is a friendly reply without any tool call.\n")+`}`,
		"stop", olUsage(240, 0, 120))
	files["ollama/minicpm-v4.6_tools.json"] = olMessage("chatcmpl-913", base+313, minicpm,
		`{"role":"assistant","content":"","reasoning":`+q(
			"Alright, forecasts for Oslo and Lima are wanted, with the tool used for both.\n\n"+
				"Only get_weather is offered and it needs a city, so one call per city.\n\n"+
				"First call with Oslo, second call with Lima.\n")+
			`,"tool_calls":[`+olToolCall("SynthCallA0000000000000000000001", 0, "Oslo")+`,`+olToolCall("SynthCallA0000000000000000000002", 1, "Lima")+`]}`,
		"tool_calls", olUsage(270, 0, 210))

	// minicpm: reasoning, then content, as a stream.
	{
		r := "We need to list the four seasons in order, starting from spring, and add one short remark. " +
			"Keep it compact and friendly.\n"
		c := "Here they are, in order: spring, summer, autumn and winter. Each one lasts about three months " +
			"in temperate regions, and then the cycle starts again."
		files["ollama/minicpm-v4.6_stream.sse"] = olStream("chatcmpl-914", base+314, minicpm, tokens(r), tokens(c), nil, "stop", olUsage(17, 0, 91))
	}

	// minicpm: long reasoning, then both tool calls in one chunk.
	{
		r := "Alright, the question covers two cities, Oslo and Lima, and asks for the tool on each.\n\n" +
			"Looking at what is available: get_weather, with a single required field, \"city\", a string. " +
			"Nothing else is needed, no units or dates.\n\n" +
			"Hmm, could one call cover both? The field takes one name, so no. Two calls it is, " +
			"and they can be issued side by side because neither depends on the other.\n\n" +
			"Double-checking the spelling: Oslo, then Lima. Both are plain names without accents, " +
			"so they can go in as written.\n\n" +
			"Order does not really matter, but following the order in the question keeps things tidy. " +
			"So the first call uses Oslo and the second uses Lima.\n\n" +
			"After the results come back, a short summary for each city will follow.\n"
		extra := []string{`"content":"","tool_calls":[` + olToolCall("SynthCallB0000000000000000000001", 0, "Oslo") + `,` +
			olToolCall("SynthCallB0000000000000000000002", 1, "Lima") + `]`}
		files["ollama/minicpm-v4.6_toolsstream.sse"] = olStream("chatcmpl-915", base+315, minicpm, tokens(r), nil, extra, "tool_calls", olUsage(270, 266, 220))
	}

	responses()
}

// ------------------------------------------------- Ollama /v1/responses

func respReasoning(id, text string) string {
	return fmt.Sprintf(`{"id":%s,"type":"reasoning","summary":[{"type":"summary_text","text":%s}],"encrypted_content":%s}`, q(id), q(text), q(text))
}

func respMessage(id, text string) string {
	return fmt.Sprintf(`{"id":%s,"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%s,"annotations":[],"logprobs":[]}]}`, q(id), q(text))
}

func respObject(id string, created int, model string, output []string, in, out, maxOut int) string {
	return fmt.Sprintf(`{"id":%s,"object":"response","created_at":%d,"completed_at":%d,"status":"completed","incomplete_details":null,"model":%s,"previous_response_id":null,"instructions":null,"output":[%s],"error":null,"tools":[],"tool_choice":"auto","truncation":"disabled","parallel_tool_calls":true,"text":{"format":{"type":"text"}},"top_p":1,"presence_penalty":0,"frequency_penalty":0,"top_logprobs":0,"temperature":1,"reasoning":null,"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"max_output_tokens":%d,"max_tool_calls":null,"store":false,"background":false,"service_tier":"default","metadata":{},"safety_identifier":null,"prompt_cache_key":null}`+"\n",
		q(id), created, created, q(model), strings.Join(output, ","), in, out, in+out, maxOut)
}

// streamResponse is the response object inside response.* stream events
// (keys sorted, as Ollama marshals it).
func streamResponse(id string, created int, completedAt, status, output, usage string, maxOut int) string {
	return fmt.Sprintf(`{"background":false,"completed_at":%s,"created_at":%d,"error":null,"frequency_penalty":0,"id":%s,"incomplete_details":null,"instructions":null,"max_output_tokens":%d,"max_tool_calls":null,"metadata":{},"model":%s,"object":"response","output":[%s],"parallel_tool_calls":true,"presence_penalty":0,"previous_response_id":null,"prompt_cache_key":null,"reasoning":null,"safety_identifier":null,"service_tier":"default","status":%s,"store":false,"temperature":1,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_logprobs":0,"top_p":1,"truncation":"disabled","usage":%s}`,
		completedAt, created, q(id), maxOut, q(gemma), output, q(status), usage)
}

func responses() {
	// The input did not fit, Ollama kept only its tail, and the model answered
	// from what it saw.
	{
		r := "The visible text begins partway through a numbered list. Its top line is \"7: red green blue\", " +
			"and the lines after it read \"entry 980: red green blue\", \"entry 981: red green blue\" and so on.\n\n" +
			"So which counts as the first entry? What is wanted is the label on the earliest visible line.\n\n" +
			"Two readings:\n- the top line, numbered 7;\n- the first line labelled \"entry\", numbered 980.\n\n" +
			"The top line is what appears first, so the answer is 7."
		files["ollama/gemma-4-e2b-4_responses_input-over-context_truncated.json"] = respObject("resp_100001", base+401, gemma,
			[]string{respReasoning("rs_resp_100001", r), respMessage("msg_100002", "7")}, 2040, 210, 400)
	}

	// max_output_tokens ran out during reasoning: empty text, yet "completed".
	{
		r := "Here is a plan for the long report on lighthouses:\n\n1.  **Scope:** The request wants a detailed, " +
			"2000-word piece on how lighthouses developed.\n\n    *   *Aim:* A clear timeline"
		files["ollama/gemma-4-e2b-4_responses_max-output-tokens.json"] = respObject("resp_100003", base+402, gemma,
			[]string{respReasoning("rs_resp_100003", r), respMessage("msg_100004", "")}, 30, 60, 60)
	}

	// The same case streamed: reasoning summary deltas only, then completed.
	{
		id, rsID, created := "resp_100005", "rs_100006", base+403
		r := "Here is a plan for the long report on lighthouses:\n\n1.  **Scope:** The request wants an in-depth, " +
			"2000-word piece on \"how lighthouses developed.\"\n\n2.  **Outline the Eras"
		var b strings.Builder
		seq := 0
		event := func(name, data string) {
			b.WriteString("event: " + name + "\ndata: " + data + "\n\n")
			seq++
		}
		seqType := func(name string) string {
			return fmt.Sprintf(`"sequence_number":%d,"type":%s}`, seq, q(name))
		}
		pending := streamResponse(id, created, "null", "in_progress", "", "null", 60)
		event("response.created", `{"response":`+pending+`,`+seqType("response.created"))
		event("response.in_progress", `{"response":`+pending+`,`+seqType("response.in_progress"))
		event("response.output_item.added", `{"item":{"id":`+q(rsID)+`,"summary":[],"type":"reasoning"},"output_index":0,`+seqType("response.output_item.added"))
		for _, d := range tokens(r) {
			event("response.reasoning_summary_text.delta", `{"delta":`+q(d)+`,"item_id":`+q(rsID)+`,"output_index":0,`+
				fmt.Sprintf(`"sequence_number":%d,"summary_index":0,"type":"response.reasoning_summary_text.delta"}`, seq))
		}
		event("response.reasoning_summary_text.done", `{"item_id":`+q(rsID)+`,"output_index":0,`+
			fmt.Sprintf(`"sequence_number":%d,"summary_index":0,"text":%s,"type":"response.reasoning_summary_text.done"}`, seq, q(r)))
		item := `{"encrypted_content":` + q(r) + `,"id":` + q(rsID) + `,"summary":[{"text":` + q(r) + `,"type":"summary_text"}],"type":"reasoning"}`
		event("response.output_item.done", `{"item":`+item+`,"output_index":0,`+seqType("response.output_item.done"))
		usage := `{"input_tokens":30,"input_tokens_details":{"cached_tokens":24},"output_tokens":60,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":90}`
		done := streamResponse(id, created+4, fmt.Sprint(created+4), "completed", item, usage, 60)
		event("response.completed", `{"response":`+done+`,`+seqType("response.completed"))
		files["ollama/gemma-4-e2b-4_responses_max-output-tokens_stream.responses.txt"] = b.String()
	}
}
