package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
)

// 协议转换由 new-api relaykit 完成（见 relay.go）；这里的回归测覆盖的是 relaykit 之上的
// 宿主行为：reasoning-only promote、usage 早报/终报形态、原生 /v1/messages 的同一条兜底。
//
// 2026-10-28 之前手写翻译翻过的车（这组测试就是为它们写的）：
//   - 流式 tool_use 整块丢失（只认 delta.text / delta.thinking）
//   - usage 只搬 output_tokens，流式 prompt_tokens 恒为早报值 8、缓存命中恒 0
//   - 只有 thinking 的一轮被交付成「空正文 + stop」，调用方守卫判 incomplete turn 狂重试

func mkServer(reasoningOnly string) *Server {
	return &Server{cfg: &Config{Upstream: UpstreamConfig{ReasoningOnly: reasoningOnly}}}
}

// sse 组装上游 Anthropic SSE 文本。
func sse(events ...string) string { return strings.Join(events, "\n") + "\n" }

const (
	evStart = `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":800,"output_tokens":0}}}`
	evThink = `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"用户要我算 391 的因数，"}}` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"391 = 17 × 23。"}}` + "\n" +
		`data: {"type":"content_block_stop","index":0}`
	evEndTurn = `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1000,"cache_creation_input_tokens":0,"cache_read_input_tokens":800,"output_tokens":42}}`
)

// streamEvent 是一次 send 调用（delta / finish / usage 任一可为 nil）。
type streamEvent struct {
	Delta  map[string]any
	Finish any
	Usage  map[string]any
}

// runStream 把 SSE 喂给 relaykit 翻译管线，按发生顺序返回所有 send 调用。
func runStream(t *testing.T, s *Server, body string) []streamEvent {
	t.Helper()
	var events []streamEvent
	send := func(d map[string]any, finish any, usage map[string]any) {
		events = append(events, streamEvent{Delta: d, Finish: finish, Usage: usage})
	}
	if _, err := s.translateRelayStream(strings.NewReader(body), send, "deepseek-v4.1-flash"); err != nil {
		t.Fatalf("translateRelayStream 失败: %v", err)
	}
	return events
}

func deltasOf(events []streamEvent) []map[string]any {
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		if e.Delta != nil {
			out = append(out, e.Delta)
		}
	}
	return out
}

func finishesOf(events []streamEvent) []any {
	var out []any
	for _, e := range events {
		if e.Finish != nil {
			out = append(out, e.Finish)
		}
	}
	return out
}

func usagesOf(events []streamEvent) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e.Usage != nil {
			out = append(out, e.Usage)
		}
	}
	return out
}

func num(x any) float64 {
	if f, ok := x.(float64); ok {
		return f
	}
	return 0
}

func contentOf(deltas []map[string]any) string {
	var b strings.Builder
	for _, d := range deltas {
		if c, ok := d["content"].(string); ok {
			b.WriteString(c)
		}
	}
	return b.String()
}

func reasoningOf(deltas []map[string]any) string {
	var b strings.Builder
	for _, d := range deltas {
		if c, ok := d["reasoning_content"].(string); ok {
			b.WriteString(c)
		}
	}
	return b.String()
}

func TestStreamPromotesThinkingOnlyStop(t *testing.T) {
	events := runStream(t, mkServer("promote"), sse(evStart, evThink, evEndTurn))
	deltas, finishes, usages := deltasOf(events), finishesOf(events), usagesOf(events)

	if got := reasoningOf(deltas); got != "用户要我算 391 的因数，391 = 17 × 23。" {
		t.Errorf("reasoning_content 应为上游 thinking 原文，得到 %q", got)
	}
	want := "用户要我算 391 的因数，391 = 17 × 23。"
	if got := contentOf(deltas); got != want {
		t.Errorf("只有 thinking 的一轮必须把 thinking 提升为正文，得到 content=%q", got)
	}
	if len(finishes) != 1 || finishes[0] != "stop" {
		t.Errorf("finish_reason 应为 stop，得到 %v", finishes)
	}
	// 提升的正文必须发在 finish 之前（先补正文再收尾），否则调用方拿不到。
	contentAt, finishAt := -1, -1
	for i, e := range events {
		if e.Finish != nil {
			finishAt = i
		}
		if c, ok := e.Delta["content"].(string); ok && c != "" {
			contentAt = i
		}
	}
	if contentAt < 0 || finishAt < 0 || contentAt > finishAt {
		t.Errorf("提升的正文要在 finish 之前发出：contentAt=%d finishAt=%d", contentAt, finishAt)
	}
	if len(usages) != 1 {
		t.Fatalf("usage 应发一次，得到 %d", len(usages))
	}
	u := usages[0]
	if num(u["prompt_tokens"]) != 1800 || num(u["completion_tokens"]) != 42 {
		t.Errorf("usage 不对: %v", u)
	}
	if det, _ := u["prompt_tokens_details"].(map[string]any); det == nil || num(det["cached_tokens"]) != 800 {
		t.Errorf("缓存命中字段丢了: %v", u["prompt_tokens_details"])
	}
}

func TestStreamPassthroughKeepsEmptyVisibleContent(t *testing.T) {
	events := runStream(t, mkServer("passthrough"), sse(evStart, evThink, evEndTurn))
	deltas, finishes := deltasOf(events), finishesOf(events)

	if got := contentOf(deltas); got != "" {
		t.Errorf("reasoning_only=passthrough 不该提升，得到 content=%q", got)
	}
	if got := reasoningOf(deltas); got == "" {
		t.Error("passthrough 也要把 reasoning_content 透传出去")
	}
	if len(finishes) != 1 || finishes[0] != "stop" {
		t.Errorf("finish_reason 应为 stop，得到 %v", finishes)
	}
}

// 上游发工具调用时，旧实现的流式翻译把它整个丢掉（只认 delta.text / delta.thinking），
// 调用方看到的就是「没有正文、也没有 tool_calls」——这就是报告里那一轮的另一种成因。
func TestStreamForwardsToolUse(t *testing.T) {
	body := sse(evStart,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"read_file"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"/tmp/a.txt\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":18}}`)

	events := runStream(t, mkServer("promote"), body)
	deltas, finishes := deltasOf(events), finishesOf(events)

	var calls []map[string]any
	for _, d := range deltas {
		raw, ok := d["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, it := range raw {
			calls = append(calls, it.(map[string]any))
		}
	}
	if len(calls) != 3 {
		t.Fatalf("应发出 3 个 tool_calls delta（首帧 + 两段参数），得到 %d: %v", len(calls), calls)
	}
	first := calls[0]
	if num(first["index"]) != 0 || first["id"] != "toolu_9" || first["type"] != "function" {
		t.Errorf("首个 delta 要带 index/id/type: %v", first)
	}
	if fn, _ := first["function"].(map[string]any); fn == nil || fn["name"] != "read_file" {
		t.Errorf("首个 delta 要带 function.name: %v", first)
	}
	// 后续两段参数必须以同一个 index 增量追加，拼起来是合法 JSON。
	args := ""
	for _, c := range calls[1:] {
		if num(c["index"]) != 0 {
			t.Errorf("参数增量必须复用 index 0，得到 %v", c["index"])
		}
		if fn, _ := c["function"].(map[string]any); fn != nil {
			s, _ := fn["arguments"].(string)
			args += s
		}
	}
	if args != `{"path":"/tmp/a.txt"}` {
		t.Errorf("参数拼接不对: %q", args)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Errorf("拼出的参数不是合法 JSON: %v", err)
	}
	if got := contentOf(deltas); got != "" {
		t.Errorf("有工具调用时不该把 thinking 提升为正文，得到 %q", got)
	}
	if len(finishes) != 1 || finishes[0] != "tool_calls" {
		t.Errorf("有工具调用时 finish_reason 必须是 tool_calls，得到 %v", finishes)
	}
}

func TestStreamFinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"max_tokens":    "length",
	}
	for upstream, want := range cases {
		body := sse(evStart,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"`+upstream+`"},"usage":{"output_tokens":3}}`)
		finishes := finishesOf(runStream(t, mkServer("promote"), body))
		if len(finishes) != 1 || finishes[0] != want {
			t.Errorf("stop_reason=%q 应映射成 %q，得到 %v", upstream, want, finishes)
		}
	}
}

// 非流式：同一轮只给 thinking，正文必须提升，reasoning_content 也要给（旧实现非流式完全不给）。
func TestWriteOpenAIPromotesThinkingOnly(t *testing.T) {
	raw := []byte(`{"id":"msg_2","type":"message","role":"assistant","stop_reason":"end_turn","content":[
		{"type":"thinking","thinking":"391 = 17 × 23。","signature":"sig"}]}`)

	rec := httptest.NewRecorder()
	mkServer("promote").writeOpenAIRelay(rec, raw, "deepseek-v4.1-flash")

	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	c := out.Choices[0]
	if c.Message.Content != "391 = 17 × 23。" {
		t.Errorf("非流式也要提升 thinking 为正文，得到 %q", c.Message.Content)
	}
	if c.Message.ReasoningContent != "391 = 17 × 23。" {
		t.Errorf("非流式应同时给 reasoning_content，得到 %q", c.Message.ReasoningContent)
	}
	if c.FinishReason != "stop" {
		t.Errorf("finish_reason 应为 stop，得到 %q", c.FinishReason)
	}

	rec2 := httptest.NewRecorder()
	mkServer("passthrough").writeOpenAIRelay(rec2, raw, "m")
	var out2 struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &out2)
	if out2.Choices[0].Message.Content != "" {
		t.Errorf("passthrough 不该提升，得到 %q", out2.Choices[0].Message.Content)
	}
	if out2.Choices[0].Message.ReasoningContent == "" {
		t.Error("passthrough 也要保留 reasoning_content")
	}
}

// 有正文的正常一轮：不能动它，不能被提升逻辑覆盖。
func TestWriteOpenAINormalTurnUntouched(t *testing.T) {
	raw := []byte(`{"id":"msg_3","type":"message","role":"assistant","stop_reason":"tool_use","content":[
		{"type":"thinking","thinking":"先看文件。"},
		{"type":"text","text":"我读一下这个文件。"},
		{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"/tmp/a.txt"}}]}`)

	rec := httptest.NewRecorder()
	mkServer("promote").writeOpenAIRelay(rec, raw, "m")

	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string           `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []map[string]any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Choices[0]
	if c.Message.Content != "我读一下这个文件。" {
		t.Errorf("正文被改写了: %q", c.Message.Content)
	}
	if c.Message.ReasoningContent != "先看文件。" {
		t.Errorf("reasoning_content 丢失: %q", c.Message.ReasoningContent)
	}
	if len(c.Message.ToolCalls) != 1 {
		t.Fatalf("工具调用丢失: %v", c.Message.ToolCalls)
	}
	if fn, _ := c.Message.ToolCalls[0]["function"].(map[string]any); fn["name"] != "read_file" {
		t.Errorf("工具名不对: %v", c.Message.ToolCalls[0])
	}
	if c.FinishReason != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，得到 %q", c.FinishReason)
	}
}

// 真的什么都没有（没有正文、没有 thinking、没有工具调用）：不动它，只记日志。
func TestStreamTrulyEmptyStaysEmpty(t *testing.T) {
	events := runStream(t, mkServer("promote"), sse(evStart, evEndTurn))
	deltas, finishes := deltasOf(events), finishesOf(events)
	if got := contentOf(deltas); got != "" {
		t.Errorf("没有 thinking 时不该凭空造正文，得到 %q", got)
	}
	if len(finishes) != 1 || finishes[0] != "stop" {
		t.Errorf("finish_reason 应为 stop，得到 %v", finishes)
	}
}

// ---- 原生 /v1/messages 的同一条兜底 ----

func TestPromoteMessagesRawInjectsTextBlock(t *testing.T) {
	raw := []byte(`{"id":"msg_9","type":"message","role":"assistant","stop_reason":"end_turn",
		"content":[{"type":"thinking","thinking":"391 = 17 × 23。","signature":"sig"}],
		"usage":{"input_tokens":10,"output_tokens":4}}`)

	out := mkServer("promote").promoteMessagesRaw(raw)
	var doc struct {
		StopReason string           `json:"stop_reason"`
		Content    []map[string]any `json:"content"`
		Usage      map[string]any   `json:"usage"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("兜底后不是合法 JSON: %v", err)
	}
	if len(doc.Content) != 2 {
		t.Fatalf("应补出一个 text 块（thinking 原样保留），得到 %d 块: %v", len(doc.Content), doc.Content)
	}
	if doc.Content[0]["type"] != "thinking" || doc.Content[0]["signature"] != "sig" {
		t.Errorf("thinking 块被改动了: %v", doc.Content[0])
	}
	if doc.Content[1]["type"] != "text" || doc.Content[1]["text"] != "391 = 17 × 23。" {
		t.Errorf("补的 text 块不对: %v", doc.Content[1])
	}
	if doc.StopReason != "end_turn" {
		t.Errorf("stop_reason 不该被改写: %q", doc.StopReason)
	}
	if doc.Usage["output_tokens"] != float64(4) {
		t.Errorf("usage 不该被动: %v", doc.Usage)
	}
}

func TestPromoteMessagesRawLeavesNormalTurnsAlone(t *testing.T) {
	cases := []string{
		// 有正文
		`{"content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"答案。"}],"stop_reason":"end_turn"}`,
		// 工具调用
		`{"content":[{"type":"tool_use","id":"t","name":"read_file","input":{}}],"stop_reason":"tool_use"}`,
		// 纯文本
		`{"content":[{"type":"text","text":"答案。"}],"stop_reason":"end_turn"}`,
	}
	for _, raw := range cases {
		if out := string(mkServer("promote").promoteMessagesRaw([]byte(raw))); out != raw {
			t.Errorf("不该改动这一轮:\n in=%s\nout=%s", raw, out)
		}
	}
	// passthrough 也不改
	onlyThinking := `{"content":[{"type":"thinking","thinking":"x"}]}`
	if out := string(mkServer("passthrough").promoteMessagesRaw([]byte(onlyThinking))); out != onlyThinking {
		t.Errorf("passthrough 不该补块: %s", out)
	}
	// 坏 JSON 不能 panic
	if out := string(mkServer("promote").promoteMessagesRaw([]byte(`not json`))); out != "not json" {
		t.Errorf("坏 JSON 应原样返回: %s", out)
	}
}

func TestPipeMessagesStreamInjectsBeforeMessageStop(t *testing.T) {
	body := sse(
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"391 = 17 × 23。"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``)

	rec := httptest.NewRecorder()
	mkServer("promote").pipeMessagesStream(rec, nil, strings.NewReader(body), "deepseek-v4.1-flash")
	out := rec.Body.String()

	// 原来那一轮必须逐字节还在
	if !strings.Contains(out, `{"type":"thinking_delta","thinking":"391 = 17 × 23。"}`) {
		t.Errorf("上游事件被改动了:\n%s", out)
	}
	idxInject := strings.Index(out, `"type":"text_delta"`)
	idxStop := strings.Index(out, `{"type":"message_stop"}`)
	if idxInject < 0 {
		t.Fatalf("没有补出 text 块，调用方还是拿不到正文:\n%s", out)
	}
	if idxStop < 0 || idxInject > idxStop {
		t.Errorf("补的文本块必须在 message_stop 之前（inject=%d stop=%d）", idxInject, idxStop)
	}
	if !strings.Contains(out, `"type":"content_block_start","index":1,"content_block":{"text":"","type":"text"}`) {
		t.Errorf("补的块应挂在下一个 index(1) 上:\n%s", out)
	}
	if !strings.Contains(out, "391 = 17 × 23。") {
		t.Errorf("补出的正文内容不对:\n%s", out)
	}
}

func TestPipeMessagesStreamLeavesVisibleTurnsAlone(t *testing.T) {
	body := sse(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想一下"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"答案在这里。"}}`,
		`data: {"type":"message_stop"}`,
		``)
	rec := httptest.NewRecorder()
	mkServer("promote").pipeMessagesStream(rec, nil, strings.NewReader(body), "m")
	if strings.Contains(rec.Body.String(), `"type":"text_delta","text":"想一下"`) {
		t.Errorf("有正文的一轮不该被补内容:\n%s", rec.Body.String())
	}
}

// 上游 message_start 里的 usage 是早报（input_tokens 还没算完、cache_read 还是 0），
// 真实数字只出现在 message_delta。relaykit 的 MergeUsageNonZero 合并两份；
// 宿主负责只把最终值下发一次（早报那份剥掉）。
func TestStreamUsageUsesFinalNumbersFromMessageDelta(t *testing.T) {
	body := sse(
		`data: {"type":"message_start","message":{"usage":{"input_tokens":8,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好。"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":42,"cache_creation_input_tokens":0,"cache_read_input_tokens":640,"output_tokens":215}}`)

	usages := usagesOf(runStream(t, mkServer("promote"), body))
	if len(usages) != 1 {
		t.Fatalf("usage 应只发一次（早报那份剥掉），得到 %d", len(usages))
	}
	u := usages[0]
	if num(u["prompt_tokens"]) != 682 {
		t.Errorf("prompt_tokens 应为 42+640=682（早报的 8 不能当最终值），得到 %v", u["prompt_tokens"])
	}
	if num(u["completion_tokens"]) != 215 || num(u["total_tokens"]) != 897 {
		t.Errorf("completion/total 不对: %v / %v", u["completion_tokens"], u["total_tokens"])
	}
	det, _ := u["prompt_tokens_details"].(map[string]any)
	if det == nil {
		t.Fatalf("缺 prompt_tokens_details: %v", u)
	}
	if num(det["cached_tokens"]) != 640 {
		t.Errorf("缓存命中字段不对: %v", det)
	}
}

// message_delta 只给 output_tokens 时（人为构造的事件形态），relaykit v0.3.5 的终值
// 就是 message_delta 那一份——早报不参与合并。真实上游的 message_delta 带完整计数
// （实测 input=42 / cache_read=640 / output=215），所以日常流量不受影响。
func TestStreamUsageFinalValueComesFromMessageDelta(t *testing.T) {
	body := sse(
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1000,"cache_read_input_tokens":800,"output_tokens":0}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好。"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`)

	usages := usagesOf(runStream(t, mkServer("promote"), body))
	if len(usages) != 1 {
		t.Fatalf("usage 应只发一次，得到 %d", len(usages))
	}
	if num(usages[0]["completion_tokens"]) != 20 {
		t.Errorf("终值应取 message_delta 的 output_tokens，得到 %v", usages[0])
	}
}

// 记账口径：billing_usage 里带着上游原始的 Anthropic 计数时，usageFromRelay 用原始值；
// 没有时从 OpenAI 口径反推。
func TestUsageFromRelayPrefersClaudeCounts(t *testing.T) {
	withBilling := &dto.Usage{
		PromptTokens: 682, CompletionTokens: 215, TotalTokens: 897,
		BillingUsage: &dto.BillingUsage{
			ClaudeUsage: &dto.ClaudeUsage{
				InputTokens: 42, CacheCreationInputTokens: 0, CacheReadInputTokens: 640, OutputTokens: 215,
			},
		},
	}
	info := usageFromRelay(withBilling)
	if info.InputTokens != 42 || info.CacheRead != 640 || info.CacheCreate != 0 || info.OutputTokens != 215 {
		t.Errorf("有 billing_usage 时应取原始 Anthropic 计数: %+v", info)
	}

	noBilling := &dto.Usage{
		PromptTokens:     931,
		CompletionTokens: 249,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 640,
		},
	}
	info = usageFromRelay(noBilling)
	if info.InputTokens != 291 || info.CacheRead != 640 || info.OutputTokens != 249 {
		t.Errorf("无 billing_usage 时应从 OpenAI 口径反推: %+v", info)
	}
}
