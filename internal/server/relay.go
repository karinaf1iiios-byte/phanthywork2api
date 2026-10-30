package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// 本文件是 new-api relaykit（AGPL-3.0，随项目整体开源）与 Phanthy 网关之间的胶水层。
// 协议转换（OpenAI Chat ⇄ Anthropic Messages，含流式、工具调用、usage 合并）全部交给
// relaykit；Phanthy 特有的宿主逻辑仍留在原处：
//   - normalizeCache：system 拼字符串 + cache_control 断点注入（chat.go）
//   - sanitizeBody：prompt 清洗（server 侧）
//   - reasoning_only promote：relaykit 只做协议转换，不做「把 thinking 当正文」这种
//     针对上游模型行为的兜底，所以它仍在宿主侧（见 translateRelayStream / writeOpenAIRelay）
//
// 替换掉的手写实现（2026-10-28 之前）：openAIToAnthropic / writeOpenAI /
// translateAnthropicStream（约 350 行）。手写版翻过两次车：流式 tool_use 整块丢失、
// usage 只搬 output_tokens 导致 prompt_tokens 恒为早报值（8）而缓存命中恒为 0。

// openAIToAnthropic 用 relaykit 把 OpenAI Chat 请求转成 Anthropic Messages 请求体。
// 返回 map 形态，后续 normalizeCache / sanitizeBody / metadata 注入直接在 map 上操作。
func (s *Server) openAIToAnthropic(in map[string]any, model string) (map[string]any, error) {
	if msgs, ok := in["messages"].([]any); ok && len(msgs) == 0 {
		return nil, fmt.Errorf("messages 为空")
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var req dto.GeneralOpenAIRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	// 转成 Claude 时请求必须有 max_tokens：上游协议里这是必填项。
	// 源请求没给就用 4096（与手写版一致）。
	meta := &convmeta.Values{
		OriginModelName:   model,
		UpstreamModelName: model,
		Options: &convmeta.Options{
			Claude: convmeta.ClaudeOptions{
				DefaultMaxTokens: func(string) int { return 4096 },
			},
		},
	}
	res, err := relayconvert.ConvertRequest(context.Background(), meta, types.RelayFormatClaude, &req)
	if err != nil {
		return nil, err
	}
	claudeReq, ok := res.Value.(*dto.ClaudeRequest)
	if !ok {
		return nil, fmt.Errorf("relaykit 转换结果不是 ClaudeRequest: %T", res.Value)
	}
	if res.Diagnostics != nil {
		for _, d := range res.Diagnostics {
			log.Printf("[chat] relaykit 请求转换诊断: %s %s", d.Code, d.Message)
		}
	}
	outBytes, err := json.Marshal(claudeReq)
	if err != nil {
		return nil, err
	}
	var anth map[string]any
	if err := json.Unmarshal(outBytes, &anth); err != nil {
		return nil, err
	}
	// Stream 是 *bool，上游要求请求体里有 "stream":true 而不是省略。
	if stream, _ := in["stream"].(bool); stream {
		if _, ok := anth["stream"]; !ok {
			anth["stream"] = true
		}
	}
	return anth, nil
}

// writeOpenAIRelay 把上游 Anthropic 响应（非流式）转成 OpenAI chat.completion 写回客户端，
// 返回这一轮的 usage（记账用）。promote 逻辑：整轮只有 thinking 时把 thinking 提为正文。
func (s *Server) writeOpenAIRelay(w http.ResponseWriter, raw []byte, model string) usageInfo {
	var resp dto.ClaudeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "解析上游响应失败: " + err.Error(),
		}})
		return usageInfo{}
	}
	res, err := relayconvert.ConvertResponse(context.Background(), nil, types.RelayFormatOpenAI, &resp)
	if err != nil {
		log.Printf("[chat] relaykit 响应转换失败: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "协议转换失败: " + err.Error(),
		}})
		return usageInfo{}
	}
	out, ok := res.Value.(*dto.OpenAITextResponse)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("relaykit 响应类型异常: %T", res.Value),
		}})
		return usageInfo{}
	}
	out.Model = model
	b, err := json.Marshal(out)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return usageInfo{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return usageInfo{}
	}
	// relaykit 不做 reasoning-only promote（那是上游模型行为，不是协议问题），宿主补。
	promoteOpenAIResponse(s.promoteThinkingOnly(), m)
	writeJSON(w, http.StatusOK, m)
	return usageFromRelay(res.Usage)
}

// translateRelayStream 把上游 Anthropic SSE 逐事件转成 OpenAI SSE delta 序列。
// send 回调签名与旧手写版一致，streamChat 不用改。
//
// 宿主在 relaykit 之上做两件事：
//  1. usage 形态修正：relaykit 会在 message_start 那一帧就带上「早报 usage」（input 还没
//     算完的那份），并在 message_delta 那一帧给最终值。OpenAI 流式惯例是只在收尾帧给
//     usage，早报那份在客户端看起来就像是「prompt_tokens=8、cached=0」，这里剥掉。
//  2. reasoning-only promote：收到 message_delta 之前若整轮没有任何正文/工具调用、只有
//     thinking，先把 thinking 当正文补一帧，避免下游守卫把「空正文 + stop」判成
//     incomplete turn（upstream.reasoning_only=promote，默认开）。
func (s *Server) translateRelayStream(rd io.Reader, send func(delta map[string]any, finish any, usage map[string]any),
	model string) (usageInfo, error) {
	ctx := context.Background()
	state, err := relayconvert.NewResponseStreamState(
		types.RelayFormatClaude, types.RelayFormatOpenAI,
		relayconvert.ResponseStreamOptions{ID: "chatcmpl-relay", Model: model, IncludeUsage: true},
	)
	if err != nil {
		return usageInfo{}, err
	}
	promote := s.promoteThinkingOnly()
	var think strings.Builder
	var sawVisible bool
	var pendingUsage map[string]any
	var contentText strings.Builder

	emit := func(chunk *dto.ChatCompletionsStreamResponse) {
		if len(chunk.Choices) == 0 {
			// 没有选择项的块（只剩 usage 的尾巴帧）：直接落 usage。
			if chunk.Usage != nil {
				ub, _ := json.Marshal(chunk.Usage)
				var u map[string]any
				if json.Unmarshal(ub, &u) == nil {
					send(map[string]any{}, nil, u)
				}
			}
			return
		}
		d := chunk.Choices[0].Delta
		delta := map[string]any{}
		if d.Role != "" {
			delta["role"] = d.Role
		}
		if d.Content != nil && *d.Content != "" {
			delta["content"] = *d.Content
			sawVisible = true
			contentText.WriteString(*d.Content)
		}
		if r := d.GetReasoningContent(); r != "" {
			delta["reasoning_content"] = r
			if !sawVisible {
				think.WriteString(r)
			}
		}
		if len(d.ToolCalls) > 0 {
			tcs := make([]any, 0, len(d.ToolCalls))
			for _, tc := range d.ToolCalls {
				tcm, _ := json.Marshal(tc)
				var t any
				if json.Unmarshal(tcm, &t) == nil {
					tcs = append(tcs, t)
				}
			}
			delta["tool_calls"] = tcs
			sawVisible = true
		}
		var finish any
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
		var usage map[string]any
		if chunk.Usage != nil {
			ub, _ := json.Marshal(chunk.Usage)
			var u map[string]any
			if json.Unmarshal(ub, &u) == nil {
				if finish == nil {
					// 早报 usage（message_start 那一帧）：暂存，不在这一帧下发。
					pendingUsage = u
				} else {
					usage = u
				}
			}
		}
		if finish != nil && usage == nil && pendingUsage != nil {
			usage = pendingUsage
		}
		if len(delta) > 0 || finish != nil || usage != nil {
			send(delta, finish, usage)
		}
	}

	sc := bufio.NewScanner(rd)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev dto.ClaudeResponse
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		// promote：必须在 relaykit 处理 message_delta（它会产出 finish 帧）之前补正文。
		if ev.Type == "message_delta" && !sawVisible && think.Len() > 0 && promote {
			promoted := think.String()
			sawVisible = true
			contentText.WriteString(promoted)
			send(map[string]any{"content": promoted}, nil, nil)
			log.Printf("[chat] 上游只回了 thinking（无正文/无工具调用），已按 upstream.reasoning_only=promote 提升为正文（%d 字，模型 %s）",
				len([]rune(promoted)), model)
		}
		results, err := relayconvert.ConvertStreamResponseChunk(ctx, nil, state, &ev)
		if err != nil {
			log.Printf("[chat] relaykit 流式事件转换失败（%s）: %v", ev.Type, err)
			continue
		}
		for _, r := range results {
			chunk, ok := r.Value.(*dto.ChatCompletionsStreamResponse)
			if !ok {
				continue
			}
			if chunk.Created == 0 {
				chunk.Created = time.Now().Unix()
			}
			emit(chunk)
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("[chat] 流读取错误: %v", err)
	}
	finals, err := relayconvert.FinalizeStreamResponse(ctx, nil, state)
	if err != nil {
		log.Printf("[chat] relaykit FinalizeStreamResponse 失败: %v", err)
	}
	for _, r := range finals {
		chunk, ok := r.Value.(*dto.ChatCompletionsStreamResponse)
		if !ok {
			continue
		}
		if chunk.Created == 0 {
			chunk.Created = time.Now().Unix()
		}
		emit(chunk)
	}
	u := usageFromRelay(state.Usage())
	if u.promptTokens() == 0 && u.OutputTokens == 0 {
		// 上游没返 usage：输出侧用 tokenkit 估算记账（输入侧拿不到请求体，不估）。
		out := estimateTokens(model, contentText.String())
		if out > 0 {
			log.Printf("[chat] 上游未返回 usage，按 tokenkit 估算输出 %d token（模型 %s）", out, model)
			u.OutputTokens = out
		}
	}
	if !sawVisible && think.Len() == 0 {
		log.Printf("[chat] 上游这一轮既没有正文也没有 reasoning（模型 %s）——空回复，交给调用方处置", model)
	}
	return u, nil
}

// usageFromRelay 把 relaykit 统一 usage（OpenAI 口径）折算回记账用的四个原始计数。
// 优先取 billing_usage 里携带的上游原始 Anthropic 计数（最准）；没有时从 OpenAI 口径反推。
func usageFromRelay(u *dto.Usage) usageInfo {
	var info usageInfo
	if u == nil {
		return info
	}
	info.OutputTokens = u.CompletionTokens
	if u.BillingUsage != nil && u.BillingUsage.ClaudeUsage != nil {
		cu := u.BillingUsage.ClaudeUsage
		info.InputTokens = cu.InputTokens
		info.CacheCreate = cu.CacheCreationInputTokens + cu.ClaudeCacheCreation5mTokens + cu.ClaudeCacheCreation1hTokens
		info.CacheRead = cu.CacheReadInputTokens
		return info
	}
	cacheCreate := u.PromptTokensDetails.CacheCreationTokensTotal()
	info.CacheRead = u.PromptTokensDetails.CachedTokens
	info.CacheCreate = cacheCreate
	info.InputTokens = u.PromptTokens - u.PromptTokensDetails.CachedTokens - cacheCreate
	if info.InputTokens < 0 {
		info.InputTokens = 0
	}
	return info
}

// promoteOpenAIResponse 在非流式 OpenAI 响应 map 上执行 reasoning-only promote：
// 正文空、没有 tool_calls、但有 reasoning_content 时，把 reasoning 提为正文。
func promoteOpenAIResponse(promote bool, m map[string]any) {
	if !promote {
		return
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	msg, ok := ch["message"].(map[string]any)
	if !ok {
		return
	}
	content, _ := msg["content"].(string)
	tcs, _ := msg["tool_calls"].([]any)
	reasoning, _ := msg["reasoning_content"].(string)
	if strings.TrimSpace(content) == "" && len(tcs) == 0 && reasoning != "" {
		msg["content"] = reasoning
		log.Printf("[chat] 上游只回了 thinking（无正文/无工具调用），已按 upstream.reasoning_only=promote 提升为正文（%d 字）",
			len([]rune(reasoning)))
	}
}
