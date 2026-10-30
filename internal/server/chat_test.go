package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"phanthywork2api/internal/session"
)

func newTestServer() *Server {
	cfg := DefaultConfig()
	return &Server{
		cfg:      cfg,
		router:   session.New(0, 0),
		scopes:   map[string]scopeEntry{},
		desk:     map[string]deskEntry{},
		cooldown: map[string]time.Time{},
	}
}

// system 数组必须拼成字符串：上游只认字符串，数组会被整段忽略。
func TestNormalizeCacheFlattensSystem(t *testing.T) {
	s := newTestServer()
	body := map[string]any{
		"system": []any{
			map[string]any{"type": "text", "text": "第一段"},
			map[string]any{"type": "text", "text": "第二段", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	flat, injected := s.normalizeCache(body)
	if !flat {
		t.Fatal("应标记已扁平化")
	}
	if _, ok := body["system"].(string); !ok {
		t.Fatalf("system 应为字符串，实际 %T", body["system"])
	}
	// system 上的断点会随扁平化消失，因此这里应当重新补一个到 messages 上
	if !injected {
		t.Fatal("system 断点丢失后应补到 messages")
	}
}

// 没有断点时补在最后一条消息的最后一个块上。
func TestNormalizeCacheInjectsBreakpoint(t *testing.T) {
	s := newTestServer()
	body := map[string]any{
		"system": "sys",
		"messages": []any{
			map[string]any{"role": "user", "content": "第一问"},
			map[string]any{"role": "assistant", "content": "第一答"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": strings.Repeat("长内容", 400)},
				map[string]any{"type": "text", "text": "第二问 B"},
			}},
		},
	}
	_, injected := s.normalizeCache(body)
	if !injected {
		t.Fatal("应补断点")
	}
	msgs := body["messages"].([]any)
	last := msgs[2].(map[string]any)["content"].([]any)
	if last[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("断点应挂在最大的块上: %+v", last[0])
	}
	if last[1].(map[string]any)["cache_control"] != nil {
		t.Fatalf("小块不该被标记: %+v", last[1])
	}
	// 前两条消息不能被改动
	if _, ok := msgs[0].(map[string]any)["content"].(string); !ok {
		t.Fatal("不该改动前面的消息")
	}
}

func TestNormalizeCacheDisabled(t *testing.T) {
	s := newTestServer()
	s.cfg.Cache.Disabled = true
	body := map[string]any{"system": "sys", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	_, injected := s.normalizeCache(body)
	if injected {
		t.Fatal("Disabled 时不该补断点")
	}
}

func TestRelocateLargeSystem(t *testing.T) {
	s := newTestServer()
	s.cfg.Cache.RelocateSystem = true
	s.cfg.Cache.SystemInlineBytes = 10
	big := "很长的系统提示词"
	body := map[string]any{
		"system":   big,
		"messages": []any{map[string]any{"role": "user", "content": "问题"}},
	}
	s.normalizeCache(body)
	if body["system"] == big {
		t.Fatal("大 system 应被搬进 messages")
	}
	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"]
	if str, ok := content.(string); ok {
		if len(str) < len(big) || str[:len(big)] != big {
			t.Fatalf("搬移后应前置原文，得到 %q", str)
		}
	} else if blocks, ok := content.([]any); ok {
		first, _ := blocks[0].(map[string]any)
		text, _ := first["text"].(string)
		if len(blocks) == 0 || len(text) < len(big) || text[:len(big)] != big {
			t.Fatalf("搬移后首块应为原文，得到 %v", blocks)
		}
	} else {
		t.Fatalf("content 形态异常: %T", content)
	}
}

func TestSessionKeyPriority(t *testing.T) {
	s := newTestServer()
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Phanthy-Session", "abc")
	if k := s.sessionKeyOf(r, map[string]any{}); k != "sid:abc" {
		t.Fatalf("应优先取会话头，得到 %q", k)
	}
	// metadata.session_id 次之
	r2 := httptest.NewRequest("POST", "/v1/messages", nil)
	body := map[string]any{"metadata": map[string]any{"session_id": "m1"}}
	if k := s.sessionKeyOf(r2, body); k != "sid:m1" {
		t.Fatalf("应取 metadata.session_id，得到 %q", k)
	}
	// 都没有时按内容哈希，且同样内容稳定
	b3 := map[string]any{"system": "S", "messages": []any{map[string]any{"role": "user", "content": "Q"}}}
	k1 := s.sessionKeyOf(httptest.NewRequest("POST", "/v1/messages", nil), b3)
	k2 := s.sessionKeyOf(httptest.NewRequest("POST", "/v1/messages", nil), b3)
	if k1 != k2 || len(k1) == 0 {
		t.Fatalf("内容哈希应稳定: %q %q", k1, k2)
	}
}

func TestStableSessionIDShape(t *testing.T) {
	id := stableSessionID("hash:deadbeef")
	if len(id) != 36 {
		t.Fatalf("应为 uuid 形态，得到 %q", id)
	}
	if id != stableSessionID("hash:deadbeef") {
		t.Fatal("同 key 应稳定")
	}
}

func TestOpenAIConversionKeepsCacheAndTools(t *testing.T) {
	s := newTestServer()
	in := map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "帮我看看"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "description": "读文件",
			"parameters": map[string]any{"type": "object"},
		}}},
		"max_tokens": float64(512),
	}
	out, err := s.openAIToAnthropic(in, "phanthy-pro")
	if err != nil {
		t.Fatal(err)
	}
	if out["system"] == nil {
		t.Fatal("system 没提出来")
	}
	// relaykit 转换后经 JSON roundtrip，数值是 float64
	if num(out["max_tokens"]) != 512 {
		t.Fatalf("max_tokens=%v", out["max_tokens"])
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 转换失败: %+v", out["tools"])
	}
	tm := tools[0].(map[string]any)
	if tm["name"] != "read_file" {
		t.Fatalf("tool name=%v", tm["name"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("system 消息应被提出去，剩 %d 条", len(msgs))
	}
}

func TestUsagePromptTokens(t *testing.T) {
	u := usageInfo{InputTokens: 4, CacheCreate: 1872, CacheRead: 0, OutputTokens: 8}
	if got := u.promptTokens(); got != 1876 {
		t.Fatalf("promptTokens=%d", got)
	}
	u.CacheRead = 100
	if got := u.promptTokens(); got != 1976 {
		t.Fatalf("promptTokens=%d", got)
	}
	b, _ := json.Marshal(u)
	if string(b) == "" {
		t.Fatal("usage 序列化失败")
	}
}
