// Package sanitize 出站净化：把「中转站/第三方 SDK」的痕迹抹掉，让请求看起来
// 就是 PhanthyWork 桌面端自己发的。
//
// 和 workbuddy2api 的 sanitize 不同，Phanthy 的桌面端本身就是 claude-code 内核，
// 所以 "You are Claude Code"、cc_version=、anthropic-beta: claude-code-20250219
// 这些恰恰是真机特征，不能删。真正暴露「不是桌面端」的是这四类：
//
//  1. 头部：x-stainless-*（SDK 版本指纹）、OpenAI 系头、UA=Go-http-client/OpenAI/Python、
//     x-api-key（桌面端只发 Authorization: Bearer）；缺 x-app / x-phanthy-client-source。
//  2. metadata.user_id：桌面端一定带 {"device_id","account_uuid","session_id"} 的 JSON 串，
//     缺省或带别人的 device_id 会被按设备维度归错组。
//  3. 会话头：x-claude-code-session-id 与实际内容不符（一个会话串多个账号/多种前缀）。
//  4. 文本里夹带的中转站标记（"x-anthropic-billing-header: ..."、"cc_entrypoint="、
//     别的厂商身份句、"sk-..." 之类），既泄露链路也会破坏上游缓存前缀的稳定性。
//
// 所有改写都是确定性的（同样输入 → 同样输出），否则会破坏 Anthropic 前缀缓存命中。
package sanitize

import (
	"regexp"
	"strings"
)

// Options 净化开关。零值 = 全开默认规则。
type Options struct {
	Disable              bool        `json:"disable"`
	KeepBillingHeader    bool        `json:"keep_billing_header"`
	ExtraRewrites        [][2]string `json:"extra_rewrites"`
	ExtraStripSubstrings []string    `json:"extra_strip_substrings"`
}

// Cleaner 可复用的净化器（正则预编译）。
type Cleaner struct {
	opts   Options
	hdrRe  *regexp.Regexp
	ccKvRe *regexp.Regexp
	skRe   *regexp.Regexp
}

// New 构造。opts.ExtraRewrites 里的非法项被忽略。
func New(opts Options) *Cleaner {
	return &Cleaner{
		opts: opts,
		// 形如 `x-anthropic-billing-header: cc_version=2.1.92.abc; cc_entrypoint=cli; cch=1a2b;`
		// 只吃 "key=value;" 形式，遇到普通文本立刻停，避免把后文一起吞掉。
		hdrRe:  regexp.MustCompile(`(?i)x-anthropic-billing-header:\s*(?:[a-z0-9_.\-]+=[^;\n]*;\s*)*`),
		ccKvRe: regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\s]*;?\s*`),
		skRe:   regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`),
	}
}

// 默认改写层：只处理「明确属于别家/中转站」的身份句，桌面端原生的 claude-code 文案保留。
var defaultRewrites = [][2]string{
	{"You are ChatGPT, a large language model trained by OpenAI", "You are a coding assistant"},
	{"as an AI language model developed by OpenAI", "as a coding assistant"},
	{"I am an AI assistant created by OpenAI", "I am a coding assistant"},
}

// Text 单段文本净化。
func (c *Cleaner) Text(s string) string {
	if c.opts.Disable || s == "" {
		return s
	}
	if !c.dirty(s) {
		return s
	}
	if !c.opts.KeepBillingHeader {
		if c.hdrRe.MatchString(s) {
			s = c.hdrRe.ReplaceAllString(s, "")
		}
	}
	if strings.Contains(s, "cc_") {
		prev := ""
		for prev != s {
			prev = s
			s = c.ccKvRe.ReplaceAllString(s, "")
		}
	}
	for _, rw := range defaultRewrites {
		s = strings.ReplaceAll(s, rw[0], rw[1])
	}
	for _, rw := range c.opts.ExtraRewrites {
		if rw[0] == "" {
			continue
		}
		s = strings.ReplaceAll(s, rw[0], rw[1])
	}
	for _, sub := range c.opts.ExtraStripSubstrings {
		if sub != "" {
			s = strings.ReplaceAll(s, sub, "")
		}
	}
	// 泄露的第三方 key 一律打码（真机不会有这种串）。
	if c.skRe.MatchString(s) {
		s = c.skRe.ReplaceAllString(s, "sk-REDACTED")
	}
	return strings.TrimSpace(s)
}

// dirty 快速预检：没有可疑特征就原样返回，避免逐请求重建大字符串。
func (c *Cleaner) dirty(s string) bool {
	if strings.Contains(s, "x-anthropic-billing-header") ||
		strings.Contains(s, "X-Anthropic-Billing-Header") ||
		strings.Contains(s, "cc_") ||
		strings.Contains(s, "sk-") {
		return true
	}
	for _, rw := range defaultRewrites {
		if strings.Contains(s, rw[0]) {
			return true
		}
	}
	for _, rw := range c.opts.ExtraRewrites {
		if rw[0] != "" && strings.Contains(s, rw[0]) {
			return true
		}
	}
	for _, sub := range c.opts.ExtraStripSubstrings {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Body 就地净化 Anthropic / OpenAI 两种请求体：
// system（字符串或 block 数组）、messages[].content（字符串或 block 数组）。
// tools 的 description 不动——它是能力声明，改了会影响模型行为。
func (c *Cleaner) Body(m map[string]any) {
	if c.opts.Disable || m == nil {
		return
	}
	if v, ok := m["system"]; ok {
		m["system"] = c.cleanContent(v)
	}
	if msgs, ok := m["messages"].([]any); ok {
		for _, item := range msgs {
			mm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// OpenAI 的 system role 消息同样处理。
			if v, ok := mm["content"]; ok {
				mm["content"] = c.cleanContent(v)
			}
		}
	}
	if p, ok := m["prompt"].(string); ok { // completions 形态
		m["prompt"] = c.Text(p)
	}
}

// cleanContent 兼容 string / []part（part.text 或 part.content 为文本）。
func (c *Cleaner) cleanContent(v any) any {
	switch t := v.(type) {
	case string:
		return c.Text(t)
	case []any:
		for _, p := range t {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := pm["text"].(string); ok {
				pm["text"] = c.Text(s)
			}
			if s, ok := pm["content"].(string); ok {
				pm["content"] = c.Text(s)
			}
		}
		return t
	default:
		return v
	}
}

// 需要从入站请求里丢掉的头部（大小写不敏感，前缀匹配用 DropPrefixes）。
var DropExact = []string{
	"x-stainless-arch", "x-stainless-lang", "x-stainless-os", "x-stainless-package-version",
	"x-stainless-runtime", "x-stainless-runtime-version", "x-stainless-retry-count",
	"x-stainless-timeout", "x-stainless-helper",
	"x-api-key", // 桌面端只发 Authorization: Bearer，带 x-api-key 像第三方 SDK
	"x-openai-organization", "openai-organization", "openai-project", "openai-beta",
	"x-ratelimit-limit-requests", "x-ratelimit-remaining-requests",
	"accept-encoding", // 由 Transport 决定
	"host", "content-length", "connection", "authorization",
}

// Drop 判断该头部是否应丢弃。
func Drop(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, d := range DropExact {
		if n == d {
			return true
		}
	}
	return strings.HasPrefix(n, "x-stainless-") || strings.HasPrefix(n, "x-openai-")
}

// FilterBeta 过滤 anthropic-beta：只保留桌面 CLI 可能发送的条目。
// 白名单外的 beta（第三方 SDK 新加的、实验性的）既泄露链路也可能改变服务端行为。
var betaAllow = map[string]bool{
	"oauth-2025-04-20":                       true,
	"claude-code-20250219":                   true,
	"interleaved-thinking-2025-05-14":        true,
	"fine-grained-tool-streaming-2025-05-14": true,
	"prompt-caching-2024-07-31":              true,
	"context-1m-2025-08-07":                  true,
	"context-management-2025-06-27":          true,
	"output-128k-2025-02-19":                 true,
	"token-efficient-tools-2025-02-19":       true,
	"files-api-2025-04-14":                   true,
}

// FilterBeta 返回过滤后的 beta 串（保序、去重）；全部被过滤时回退默认两条。
func FilterBeta(v string) string {
	if strings.TrimSpace(v) == "" {
		return "claude-code-20250219,oauth-2025-04-20"
	}
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(v, ",") {
		p := strings.TrimSpace(part)
		if p == "" || seen[p] || !betaAllow[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return "claude-code-20250219,oauth-2025-04-20"
	}
	if !seen["oauth-2025-04-20"] {
		out = append(out, "oauth-2025-04-20")
	}
	return strings.Join(out, ",")
}
