package sanitize

import (
	"encoding/json"
	"strings"
	"testing"
)

func clean(t *testing.T, in string) string {
	t.Helper()
	return New(Options{}).Text(in)
}

func TestStripBillingHeader(t *testing.T) {
	in := `前文 x-anthropic-billing-header: cc_version=2.1.92.abc; cc_entrypoint=cli; cch=1a2b; 后文`
	out := clean(t, in)
	if strings.Contains(out, "x-anthropic-billing-header") || strings.Contains(out, "cc_version=") {
		t.Fatalf("未清理: %q", out)
	}
	if !strings.Contains(out, "前文") || !strings.Contains(out, "后文") {
		t.Fatalf("误删正文: %q", out)
	}
}

func TestRewriteForeignIdentity(t *testing.T) {
	out := clean(t, "You are ChatGPT, a large language model trained by OpenAI. Be nice.")
	if strings.Contains(out, "OpenAI") {
		t.Fatalf("未改写: %q", out)
	}
}

func TestKeepNativeClaudeCodeWording(t *testing.T) {
	// Phanthy 桌面端内核就是 claude-code，这句是真机特征，必须保留。
	in := "You are Claude Code, Anthropic's official CLI for Claude."
	if out := clean(t, in); out != in {
		t.Fatalf("不该改写原生文案: %q", out)
	}
}

func TestRedactThirdPartyKey(t *testing.T) {
	out := clean(t, "key=sk-abcdefghijklmnopqrstuvwx rest")
	if strings.Contains(out, "abcdefghijklmnopqrstuvwx") || !strings.Contains(out, "sk-REDACTED") {
		t.Fatalf("未打码: %q", out)
	}
}

func TestBodyWalksSystemAndMessages(t *testing.T) {
	var body map[string]any
	raw := `{"system":[{"type":"text","text":"a x-anthropic-billing-header: cc_version=1; b"}],
	         "messages":[{"role":"user","content":[{"type":"text","text":"cc_entrypoint=cli; hi"}]},
	                     {"role":"user","content":"plain"}]}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	New(Options{}).Body(body)
	out, _ := json.Marshal(body)
	s := string(out)
	if strings.Contains(s, "x-anthropic-billing-header") || strings.Contains(s, "cc_entrypoint=") {
		t.Fatalf("body 未清理: %s", s)
	}
	if !strings.Contains(s, "plain") || !strings.Contains(s, "hi") {
		t.Fatalf("误删内容: %s", s)
	}
}

func TestDisabledIsNoop(t *testing.T) {
	in := "x-anthropic-billing-header: cc_version=1;"
	if out := New(Options{Disable: true}).Text(in); out != in {
		t.Fatal("Disable 应不改动")
	}
}

func TestDropHeaders(t *testing.T) {
	for _, h := range []string{"x-stainless-os", "X-Stainless-Lang", "x-api-key", "x-openai-project"} {
		if !Drop(h) {
			t.Fatalf("%s 应被丢弃", h)
		}
	}
	for _, h := range []string{"x-app", "anthropic-beta", "x-phanthy-session"} {
		if Drop(h) {
			t.Fatalf("%s 不该丢", h)
		}
	}
}

func TestFilterBeta(t *testing.T) {
	got := FilterBeta("oauth-2025-04-20,some-experimental-2099,claude-code-20250219")
	if strings.Contains(got, "some-experimental") {
		t.Fatalf("白名单外的 beta 应被过滤: %s", got)
	}
	if !strings.Contains(got, "oauth-2025-04-20") || !strings.Contains(got, "claude-code-20250219") {
		t.Fatalf("白名单内应保留: %s", got)
	}
	if got := FilterBeta("weird-beta"); got != "claude-code-20250219,oauth-2025-04-20" {
		t.Fatalf("全被过滤时应回退默认: %s", got)
	}
}
