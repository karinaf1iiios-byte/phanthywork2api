// chat.go 推理面：会话粘性路由 + 缓存命中透传 + 出站净化。
//
// 三条主线：
//
//  1. 粘性：同一会话固定一个账号（见 internal/session）。会话 key 取自请求头
//     x-phanthy-session / X-Claude-Code-Session-Id，其次 metadata.session_id、
//     conversation_id，最后退化为「系统提示 + 首条用户消息」的哈希。
//  2. 缓存：Anthropic 的前缀缓存靠 cache_control 断点，命中与否只体现在
//     usage.cache_read_input_tokens。网关的责任是
//     ——原样保留调用方给的 cache_control（不重排、不裁剪 content 块）；
//     ——调用方没给时在 system 末尾补一个 ephemeral 断点（真机 claude-code 就这么做）；
//     ——把 cache_read / cache_creation 原样透传给下游（OpenAI 形态映射成
//     prompt_tokens_details.cached_tokens），并计入日志，方便核算真实成本。
//  3. 净化：见 internal/sanitize。请求体走一次确定性改写，头部按白名单重建。
package server

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/sanitize"
	"phanthywork2api/internal/upstream"
)

// ---------------- 账号/身份缓存 ----------------

type scopeEntry struct {
	scope string
	exp   time.Time
}

type deskEntry struct {
	token string
	exp   time.Time
}

func (s *Server) scopeOf(a *auth.Auth) (string, error) {
	s.mu.Lock()
	if e, ok := s.scopes[a.UID]; ok && time.Now().Before(e.exp) {
		s.mu.Unlock()
		return e.scope, nil
	}
	s.mu.Unlock()
	scope, err := s.accountScope(a, "")
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.scopes == nil {
		s.scopes = map[string]scopeEntry{}
	}
	s.scopes[a.UID] = scopeEntry{scope: scope, exp: time.Now().Add(30 * time.Minute)}
	s.mu.Unlock()
	return scope, nil
}

// desktopFor 返回该账号的推理通道句柄：Activities + access token + desktop_inference_token。
// desktop token 是 180s 的 JWT，这里缓存 90s，留足签名与网络往返余量。
func (s *Server) desktopFor(a *auth.Auth) (*upstream.Activities, string, string, error) {
	scope, err := s.scopeOf(a)
	if err != nil {
		return nil, "", "", err
	}
	act, err := s.activities(a)
	if err != nil {
		return nil, "", "", err
	}
	// 请求前兜底：keepalive 之外的第二道保险。空闲号靠 keepalive，正在干活的号
	// 靠这里——否则 token 一过期就只能等上游 401 再换号（推理路径原本完全不刷新）。
	s.refreshIfNeeded(a, 2*time.Minute)
	a.Lock()
	token := a.AccessToken
	a.Unlock()
	if token == "" {
		return nil, "", "", fmt.Errorf("账号 %s 无 accessToken", a.UID)
	}
	s.mu.Lock()
	e, ok := s.desk[a.UID]
	s.mu.Unlock()
	if ok && time.Now().Before(e.exp) {
		return act, token, e.token, nil
	}
	summ, _, _, err := act.Summary(token)
	if err != nil {
		return nil, "", "", err
	}
	if summ.DesktopInferenceToken == "" {
		return nil, "", "", fmt.Errorf("summary 未返回 desktop_inference_token（scope=%s）", scope)
	}
	s.mu.Lock()
	if s.desk == nil {
		s.desk = map[string]deskEntry{}
	}
	s.desk[a.UID] = deskEntry{token: summ.DesktopInferenceToken, exp: time.Now().Add(90 * time.Second)}
	s.mu.Unlock()
	// 每次取证明顺带刷新积分额度：权重里"额度因子"要用最新值，不能靠上一轮缓存。
	a.SetUsage(int64(summ.Credits.Available), 0)
	return act, token, summ.DesktopInferenceToken, nil
}

// ---------------- 会话 key 与粘性选择 ----------------

// sessionKeyOf 提取会话标识。优先级与真机一致：会话头 > metadata.session_id > 内容哈希。
func (s *Server) sessionKeyOf(r *http.Request, body map[string]any) string {
	for _, h := range []string{"X-Phanthy-Session", "X-Claude-Code-Session-Id", "X-Session-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return "sid:" + v
		}
	}
	if body != nil {
		if md, ok := body["metadata"].(map[string]any); ok {
			if v, ok := md["session_id"].(string); ok && v != "" {
				return "sid:" + v
			}
		}
		for _, k := range []string{"conversation_id", "conversation", "session_id", "user"} {
			if v, ok := body[k].(string); ok && v != "" {
				return k + ":" + v
			}
		}
		// 真机没有显式会话 id 时（curl / 简单 SDK），按内容前缀取指纹，
		// 至少保证同一段对话落同一个账号，缓存与追问都不至于断。
		h := sha256.New()
		if sys, ok := body["system"].(string); ok {
			h.Write([]byte(sys))
		}
		if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
			if b, err := json.Marshal(msgs[0]); err == nil {
				h.Write(b)
			}
		}
		return "hash:" + hex.EncodeToString(h.Sum(nil)[:8])
	}
	return ""
}

// pickSticky 按会话选账号；失败账号进冷却表，避免整段会话反复撞同一颗雷。
func (s *Server) pickSticky(key, model string) (*auth.Auth, bool, error) {
	cands := s.usableAccounts()
	if len(cands) == 0 {
		return nil, false, fmt.Errorf("没有可用账号（auths/ 为空或全部冷却中）")
	}
	choose := func() string {
		// 三因子加权抽签（额度 / 快过期 / 闲置），见 weight.go。
		// 冷却中的号先排除；全在冷却时退回全集（下面还会提前解冻一次）。
		avail := make([]*auth.Auth, 0, len(cands))
		for _, c := range cands {
			if !s.cooling(c.UID) {
				avail = append(avail, c)
			}
		}
		if len(avail) == 0 {
			avail = cands
		}
		if pick := s.pickWeighted(avail, nil); pick != nil {
			return pick.UID
		}
		return cands[0].UID
	}
	bindOK := func(uid string) bool { return !s.cooling(uid) && s.accountByUID(uid) != nil }
	uid, sticky := s.router.Pick(key, model, choose, bindOK)
	a := s.accountByUID(uid)
	if a == nil {
		s.router.Drop(key)
		return nil, false, fmt.Errorf("选中的账号 %s 已消失", uid)
	}
	if s.cooling(uid) && len(cands) > 1 {
		s.cool(uid, 0) // 提前解除再试一次，真挂了会在下面重新冷却
	}
	return a, sticky, nil
}

func (s *Server) usableAccounts() []*auth.Auth {
	out := make([]*auth.Auth, 0, len(s.auths))
	for _, a := range s.auths {
		if a == nil {
			continue
		}
		a.Lock()
		ok := a.AccessToken != "" || a.RefreshToken != ""
		a.Unlock()
		if ok {
			out = append(out, a)
		}
	}
	return out
}

func (s *Server) accountByUID(uid string) *auth.Auth {
	for _, a := range s.auths {
		if a.UID == uid {
			return a
		}
	}
	return nil
}

func (s *Server) cooling(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.cooldown[uid]
	return ok && time.Now().Before(until)
}

func (s *Server) cool(uid string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooldown == nil {
		s.cooldown = map[string]time.Time{}
	}
	if d <= 0 {
		delete(s.cooldown, uid)
		return
	}
	s.cooldown[uid] = time.Now().Add(d)
}

// ---------------- 请求体构造 ----------------

// openAIToAnthropic 把 OpenAI chat 请求转成 Anthropic Messages 请求体。
// 关键：system 单独提到 system 数组；保留 tools；cache_control 断点补在 system 末尾。
// normalizeCache 处理两件 Phanthy 特有的事（实测 2026-10-28）：
//
//  1. 服务端只认 system 字符串。传 [{"type":"text","text":...}] 数组会被整段忽略
//     （问「system 里的暗号是什么」答不出来，input_tokens 只剩几百）。
//     所以数组必须拼成字符串再发，否则系统提示词等于没发。
//  2. 缓存断点必须落在 messages 的 content 块上：system 变成字符串后没法挂
//     cache_control，挂到末尾用户消息的最后一个块上，前缀照样命中
//     （实测 cc=2564 → 下一轮 cr=2564）。
//
// 调用方自己已经给了 cache_control 时只做 (1)，不动它的断点位置。
func (s *Server) normalizeCache(body map[string]any) (flattened, injected bool) {
	if sys, ok := body["system"].([]any); ok {
		parts := make([]string, 0, len(sys))
		for _, p := range sys {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			} else if t, ok := p.(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
		body["system"] = strings.Join(parts, "\n\n")
		flattened = true
	}
	// 实测：system 很大时上游只写不读缓存；把大段内容搬进 messages 才能命中。
	if s.cfg.Cache.RelocateSystem {
		if txt, ok := body["system"].(string); ok && txt != "" {
			limit := s.cfg.Cache.SystemInlineBytes
			if limit <= 0 {
				limit = 4096
			}
			if len(txt) > limit {
				if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
					if m, ok := msgs[0].(map[string]any); ok && m["role"] == "user" {
						m["content"] = prependText(m["content"], txt)
						msgs[0] = m
						body["messages"] = msgs
						body["system"] = "You are a helpful coding assistant. Follow the project context in the conversation."
						flattened = true
					}
				}
			}
		}
	}
	// 断点只能挂在 messages 的块上：system 上的断点在扁平化时必然丢失，
	// 所以这里只看 messages 里有没有，没有就补一个。
	if s.cfg.Cache.Disabled || hasMessageCacheControl(body) {
		return flattened, false
	}
	// 断点要挂在「有份量的块」上：实测挂在尾部 2 token 的小块上时，
	// 每轮都是 cache_creation（2328）而 cache_read 恒为 0——上游按块内容建缓存，
	// 小块撑不起一个可命中的前缀条目。所以从最后一条消息往前找最大的文本块。
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			if len(c) < 800 && i > 0 {
				continue
			}
			m["content"] = []any{map[string]any{"type": "text", "text": c, "cache_control": map[string]any{"type": "ephemeral"}}}
			msgs[i] = m
			return flattened, true
		case []any:
			if len(c) == 0 {
				continue
			}
			best, bestLen := -1, 0
			for j, b := range c {
				bm, ok := b.(map[string]any)
				if !ok || bm["type"] != "text" {
					continue
				}
				if t, ok := bm["text"].(string); ok && len(t) > bestLen {
					best, bestLen = j, len(t)
				}
			}
			if best < 0 {
				continue
			}
			if bestLen < 800 && i > 0 {
				continue // 本消息里没有够大的块，往前找
			}
			bb := c[best].(map[string]any)
			bb["cache_control"] = map[string]any{"type": "ephemeral"}
			c[best] = bb
			m["content"] = c
			msgs[i] = m
			return flattened, true
		}
	}
	return flattened, false
}

// prependText 把 text 放在消息内容最前面（字符串或 block 数组两种形态都支持）。
func prependText(content any, text string) any {
	switch c := content.(type) {
	case string:
		return text + "\n\n" + c
	case []any:
		return append([]any{map[string]any{"type": "text", "text": text}}, c...)
	default:
		return text
	}
}

func hasMessageCacheControl(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for _, item := range msgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if blocks, ok := m["content"].([]any); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok {
					if _, has := bm["cache_control"]; has {
						return true
					}
				}
			}
		}
	}
	return false
}

func hasCacheControl(body map[string]any) bool {
	if s, ok := body["system"].([]any); ok {
		for _, p := range s {
			if pm, ok := p.(map[string]any); ok {
				if _, has := pm["cache_control"]; has {
					return true
				}
			}
		}
	}
	msgs, _ := body["messages"].([]any)
	for _, item := range msgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if blocks, ok := m["content"].([]any); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok {
					if _, has := bm["cache_control"]; has {
						return true
					}
				}
			}
		}
	}
	return false
}

// usageOf 从 Anthropic 响应里抽 usage，并把缓存命中拆出来。
type usageInfo struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CacheCreate  int `json:"cache_creation_input_tokens"`
	CacheRead    int `json:"cache_read_input_tokens"`
}

func parseUsage(raw []byte) usageInfo {
	var doc struct {
		Usage usageInfo `json:"usage"`
	}
	_ = json.Unmarshal(raw, &doc)
	return doc.Usage
}

// promptTokens OpenAI 口径：输入总量 = 未命中 + 新建 + 命中。
func (u usageInfo) promptTokens() int { return u.InputTokens + u.CacheCreate + u.CacheRead }

func (u usageInfo) logLine(model, uid string, sticky bool) string {
	total := u.promptTokens()
	hit := 0.0
	if total > 0 {
		hit = float64(u.CacheRead) * 100 / float64(total)
	}
	return fmt.Sprintf("[chat] model=%s acct=%s sticky=%v in=%d cache_read=%d cache_new=%d out=%d hit=%.1f%%",
		model, uid, sticky, u.InputTokens, u.CacheRead, u.CacheCreate, u.OutputTokens, hit)
}

// ---------------- OpenAI 兼容端点 ----------------

func (s *Server) handlerChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "JSON 解析失败: " + err.Error()}})
		return
	}
	clientModel, _ := in["model"].(string)
	model, okModel := s.mapModel(clientModel)
	if !okModel {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"code": "model_not_allowed", "message": "未知模型（upstream.strict_models=true）：" + clientModel}})
		return
	}
	key := s.sessionKeyOf(r, in)
	stream, _ := in["stream"].(bool)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		a, sticky, err := s.pickSticky(key, model)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": "no_account", "message": err.Error()}})
			return
		}
		act, token, dtoken, err := s.desktopFor(a)
		if err != nil {
			lastErr = err
			s.cool(a.UID, 30*time.Second)
			s.router.Drop(key)
			continue
		}
		anth, err := s.openAIToAnthropic(in, model)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		s.normalizeCache(anth)
		s.sanitizeBody(anth)
		prof := act.Identity().DeviceProfile()
		accountUUID := act.Identity().AccountScope
		// metadata.user_id：桌面端一定带，且 device_id 固定为这台机器。
		md, _ := anth["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		md["user_id"] = prof.MetadataUserID(accountUUID, strings.TrimPrefix(key, "sid:"))
		anth["metadata"] = md

		if os.Getenv("P2A_DEBUG_BODY") == "1" {
			b, _ := json.Marshal(anth)
			log.Printf("[chat] 出站体: %s", truncate(string(b), 600))
		}
		bodyBytes, err := json.Marshal(anth)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		opt := upstream.InferenceOpt{
			SessionID:   strings.TrimPrefix(key, "sid:"),
			Profile:     prof,
			MaxBytes:    32 << 20,
			HTTPTimeout: 180 * time.Second,
		}
		if !strings.HasPrefix(key, "sid:") {
			// 没有显式会话 id 时也要给服务端一个稳定的会话串，
			// 否则同一段对话在服务端看来每次都是新会话，追问与缓存都算不出来。
			opt.SessionID = stableSessionID(key)
		}

		if stream {
			s.streamChat(w, r, act, token, dtoken, bodyBytes, opt, model, a.UID, sticky)
			return
		}
		st, raw, hdr, err := act.MessagesH(token, dtoken, bodyBytes, opt)
		if err != nil {
			lastErr = err
			s.cool(a.UID, 30*time.Second)
			s.router.Drop(key)
			continue
		}
		if st != http.StatusOK {
			// 会话相关失败换账号重试；鉴权失败清 token 缓存
			if st == 401 || st == 403 || st == 429 || st == 502 || st == 503 {
				log.Printf("[chat] %s http %d: %s", a.UID, st, truncate(string(raw), 300))
				s.router.Drop(key)
				if st == 401 {
					s.mu.Lock()
					delete(s.desk, a.UID)
					s.mu.Unlock()
				}
				s.cool(a.UID, 20*time.Second)
				lastErr = fmt.Errorf("上游 %d: %s", st, truncate(string(raw), 200))
				continue
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st)
			_, _ = w.Write(raw)
			return
		}
		if cp := hdr.Get("x-phanthy-cost-points"); cp != "" {
			log.Printf("[chat] 本次扣 %s 积分（%s）", cp, a.UID)
		}
		u := s.writeOpenAIRelay(w, raw, model)
		if u.promptTokens() == 0 && u.OutputTokens == 0 {
			// 上游没返 usage（实测没遇到过，留作兜底）：用 tokenkit 估算记账，交付给客户端的 usage 不动。
			in := estimateTokens(model, anthropicText(anth))
			out := estimateTokens(model, claudeResponseText(raw))
			log.Printf("[chat] %s 上游未返回 usage，按 tokenkit 估算记账 in=%d out=%d", a.UID, in, out)
			u.InputTokens, u.OutputTokens = in, out
		}
		log.Print(u.logLine(model, a.UID, sticky))
		s.tally(u)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": fmt.Sprintf("推理失败: %v", lastErr)}})
}

// stableSessionID 由会话 key 派生一个 uuid 形态的稳定 id（同一对话始终相同）。
func stableSessionID(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Server) tally(u usageInfo) {
	atomic.AddInt64(&s.statPrompt, int64(u.promptTokens()))
	atomic.AddInt64(&s.statCacheRead, int64(u.CacheRead))
	atomic.AddInt64(&s.statCacheNew, int64(u.CacheCreate))
}

// streamChat 把 Anthropic SSE 翻成 OpenAI SSE（逐事件转发，不缓冲整段）。
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, act *upstream.Activities,
	token, dtoken string, body []byte, opt upstream.InferenceOpt, model, uid string, sticky bool) {
	resp, err := act.MessagesStream(token, dtoken, body, opt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		log.Printf("[chat] stream %s http %d: %s", uid, resp.StatusCode, truncate(string(raw), 300))
		s.router.Drop(sessionKeyFromOpt(opt))
		s.cool(uid, 20*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-" + hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))[:16]
	send := func(delta map[string]any, finish any, usage map[string]any) {
		chunk := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	u, _ := s.translateRelayStream(resp.Body, send, model)
	log.Print(u.logLine(model, uid, sticky))
	s.tally(u)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	_ = r
}

func splitReasoningOnly(content []map[string]any) (string, bool) {
	var thinking strings.Builder
	visible := false
	for _, b := range content {
		switch fmt.Sprint(b["type"]) {
		case "thinking":
			if t, ok := b["thinking"].(string); ok {
				thinking.WriteString(t)
			}
		case "text":
			if t, _ := b["text"].(string); strings.TrimSpace(t) != "" {
				visible = true
			}
		case "tool_use":
			visible = true
		}
	}
	return thinking.String(), !visible && thinking.Len() > 0
}

// promoteMessagesRaw 给原生 /v1/messages 的非流式响应做同一件事：只有 thinking 时补一个
// text 块（thinking 块原样保留）。解析失败就原样返回，绝不因为兜底把正常响应弄坏。
func (s *Server) promoteMessagesRaw(raw []byte) []byte {
	if !s.promoteThinkingOnly() {
		return raw
	}
	var probe struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Content == nil {
		return raw
	}
	thinking, onlyReasoning := splitReasoningOnly(probe.Content)
	if !onlyReasoning {
		return raw
	}
	var whole map[string]any
	if err := json.Unmarshal(raw, &whole); err != nil {
		return raw
	}
	content, _ := whole["content"].([]any)
	whole["content"] = append(content, map[string]any{"type": "text", "text": thinking})
	out, err := json.Marshal(whole)
	if err != nil {
		return raw
	}
	log.Printf("[messages] 上游只回了 thinking（无正文/无工具调用），已按 upstream.reasoning_only=promote 补一个 text 块（%d 字）",
		len([]rune(thinking)))
	return out
}

// pipeMessagesStream 逐行透传上游 SSE（字节不改），只在收尾时兜底：
// 整轮没有任何 text / tool_use 块、只发了 thinking → 在 message_stop 之前补一组
// content_block_start / content_block_delta / content_block_stop，把 thinking 当正文交付。
func (s *Server) pipeMessagesStream(w http.ResponseWriter, flusher http.Flusher, body io.Reader, model string) {
	promote := s.promoteThinkingOnly()
	rd := bufio.NewReaderSize(body, 64*1024)
	var think strings.Builder
	visible := false
	blocks := 0
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimRight(line, "\r\n"), "data:"))
			if strings.HasPrefix(strings.TrimSpace(line), "data:") && payload != "" && payload != "[DONE]" {
				var ev struct {
					Type         string `json:"type"`
					ContentBlock struct {
						Type string `json:"type"`
					} `json:"content_block"`
					Delta struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						Thinking string `json:"thinking"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(payload), &ev) == nil {
					switch ev.Type {
					case "content_block_start":
						blocks++
						if ev.ContentBlock.Type == "text" || ev.ContentBlock.Type == "tool_use" {
							visible = true
						}
					case "content_block_delta":
						if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
							visible = true
						}
						if ev.Delta.Thinking != "" {
							think.WriteString(ev.Delta.Thinking)
						}
					case "message_stop":
						if !visible && think.Len() > 0 && promote {
							text := think.String()
							log.Printf("[messages] 上游只回了 thinking（无正文/无工具调用），已按 upstream.reasoning_only=promote 补一个 text 块（%d 字，模型 %s）",
								len([]rune(text)), model)
							writeSSEBlock(w, blocks, text)
							if flusher != nil {
								flusher.Flush()
							}
						}
					}
				}
			}
			_, _ = io.WriteString(w, line)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// writeSSEBlock 以 index 补一个完整的 text 内容块（Anthropic SSE 形态）。
func writeSSEBlock(w io.Writer, index int, text string) {
	b, _ := json.Marshal(map[string]any{"type": "text", "text": ""})
	fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\n", index, b)
	d, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text}})
	fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", d)
	fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index)
}

// promoteThinkingOnly 是否把「只有 thinking」的一轮提升成正文（默认是）。
// 配 upstream.reasoning_only = "passthrough" 可关掉，恢复原样透传。
func (s *Server) promoteThinkingOnly() bool {
	return strings.ToLower(strings.TrimSpace(s.cfg.Upstream.ReasoningOnly)) != "passthrough"
}

func sessionKeyFromOpt(opt upstream.InferenceOpt) string {
	if opt.SessionID == "" {
		return ""
	}
	return "sid:" + opt.SessionID
}

// sanitizeBody 出站净化（文本层）。
func (s *Server) sanitizeBody(body map[string]any) {
	if s.cleaner == nil {
		s.cleaner = sanitize.New(sanitize.Options{
			Disable:              s.cfg.Sanitize.Disabled,
			KeepBillingHeader:    s.cfg.Sanitize.KeepBillingHeader,
			ExtraRewrites:        s.cfg.Sanitize.ExtraRewrites,
			ExtraStripSubstrings: s.cfg.Sanitize.ExtraStrip,
		})
	}
	s.cleaner.Body(body)
}

// ---------------- 原生 Anthropic 端点 ----------------

// handlerMessages 原生 /v1/messages 透传：给 Claude Code / Anthropic SDK 直接用。
// 除了模型名映射与会话粘性，请求体与响应体（含流）都不改动，缓存断点原样保留。
func (s *Server) handlerMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "JSON 解析失败: " + err.Error()}})
		return
	}
	clientModel, _ := in["model"].(string)
	model, okModel := s.mapModel(clientModel)
	if !okModel {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"type": "invalid_request_error", "code": "model_not_allowed",
			"message": "未知模型（upstream.strict_models=true）：" + clientModel}})
		return
	}
	in["model"] = model
	// 调用方没给 cache_control 时补一个 system 末尾断点：不这么做，同样的长前缀
	// 每次都是全价重算（cache_read=0），中转场景下成本差 5-10 倍。
	s.normalizeCache(in)
	key := s.sessionKeyOf(r, in)
	stream, _ := in["stream"].(bool)

	for attempt := 0; attempt < 2; attempt++ {
		a, sticky, err := s.pickSticky(key, model)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"type": "error", "message": err.Error()}})
			return
		}
		act, token, dtoken, err := s.desktopFor(a)
		if err != nil {
			s.cool(a.UID, 20*time.Second)
			s.router.Drop(key)
			continue
		}
		s.sanitizeBody(in)
		prof := act.Identity().DeviceProfile()
		md, _ := in["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		if _, ok := md["user_id"]; !ok || md["user_id"] == "" {
			md["user_id"] = prof.MetadataUserID(act.Identity().AccountScope, strings.TrimPrefix(key, "sid:"))
			in["metadata"] = md
		}
		bodyBytes, err := json.Marshal(in)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		opt := upstream.InferenceOpt{
			SessionID:   optSession(key),
			Profile:     prof,
			Beta:        sanitize.FilterBeta(r.Header.Get("anthropic-beta")),
			HTTPTimeout: 180 * time.Second,
		}
		if stream {
			resp, err := act.MessagesStream(token, dtoken, bodyBytes, opt)
			if err != nil {
				s.cool(a.UID, 20*time.Second)
				s.router.Drop(key)
				continue
			}
			defer resp.Body.Close()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(resp.StatusCode)
			flusher, _ := w.(http.Flusher)
			s.pipeMessagesStream(w, flusher, resp.Body, model)
			return
		}
		st, raw, hdr, err := act.MessagesH(token, dtoken, bodyBytes, opt)
		if err != nil {
			s.cool(a.UID, 20*time.Second)
			s.router.Drop(key)
			continue
		}
		if st == 401 || st == 403 || st == 429 {
			log.Printf("[messages] %s http %d: %s", a.UID, st, truncate(string(raw), 300))
			s.router.Drop(key)
			s.cool(a.UID, 20*time.Second)
			continue
		}
		u := parseUsage(raw)
		if st == http.StatusOK {
			a.MarkUsed(time.Now().Unix()) // 闲置因子：这个号刚干过活
			log.Print(u.logLine(model, a.UID, sticky))
			if cp := hdr.Get("x-phanthy-cost-points"); cp != "" {
				log.Printf("[messages] 本次扣 %s 积分（%s）", cp, a.UID)
			}
			s.tally(u)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-p2a-account", a.UID)
		w.Header().Set("x-p2a-session-sticky", fmt.Sprint(sticky))
		if u.CacheRead > 0 || u.CacheCreate > 0 {
			w.Header().Set("x-p2a-cache-read", fmt.Sprint(u.CacheRead))
			w.Header().Set("x-p2a-cache-new", fmt.Sprint(u.CacheCreate))
		}
		w.WriteHeader(st)
		if st == http.StatusOK {
			raw = s.promoteMessagesRaw(raw)
		}
		_, _ = w.Write(raw)
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "推理失败（已换账号重试）"}})
}

// optSession 把会话 key 归一成会话 id（hash: 前缀也映射成稳定 uuid）。
func optSession(key string) string {
	if strings.HasPrefix(key, "sid:") {
		return strings.TrimPrefix(key, "sid:")
	}
	return stableSessionID(key)
}
