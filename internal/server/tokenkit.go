package server

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/tokenkit"
)

// 本地 token 计数（new-api tokenkit，AGPL-3.0）：上游不返 usage 时的记账兜底，
// 以及调试/成本预估。真实计量仍以上游返回的 usage 为准（见 relay.go 的 usageFromRelay）。
//
// tokenkit.Estimate 对 OpenAI 文本模型走 tiktoken 精确计数，其余按字符类型加权估算
// （CJK / emoji / 数学符号 / URL 各有权重）。我们这边(model=phanthy-pro 之类)走估算分支。

// estimateTokens 估算一段文本的 token 数。纯计算，不会失败。
func estimateTokens(model, text string) int {
	if text == "" {
		return 0
	}
	return tokenkit.Estimate(model, text)
}

// anthropicText 从转换后的 Anthropic 请求体里抽出全部文本（system + 各消息内容），
// 用于输入侧估算。
func anthropicText(anth map[string]any) string {
	var b strings.Builder
	switch sys := anth["system"].(type) {
	case string:
		b.WriteString(sys)
	case []any:
		for _, p := range sys {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
	}
	if msgs, ok := anth["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			switch c := mm["content"].(type) {
			case string:
				b.WriteString(c)
			case []any:
				for _, blk := range c {
					bm, ok := blk.(map[string]any)
					if !ok {
						continue
					}
					if t, ok := bm["text"].(string); ok {
						b.WriteString(t)
					}
					if t, ok := bm["thinking"].(string); ok {
						b.WriteString(t)
					}
				}
			}
		}
	}
	return b.String()
}

// claudeResponseText 从上游 Anthropic 响应里抽出正文与 thinking 文本，
// 用于输出侧估算。
func claudeResponseText(raw []byte) string {
	var doc struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &doc)
	var b strings.Builder
	for _, c := range doc.Content {
		if c.Type == "text" || c.Type == "thinking" {
			b.WriteString(c.Text)
			b.WriteString(c.Thinking)
		}
	}
	return b.String()
}
