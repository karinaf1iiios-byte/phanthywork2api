// handler.go：HTTP 端点实现（health / status / models / chat / phanthy 只读代理）。
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

// ---------- health ----------

func (s *Server) handlerHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "accounts": len(s.auths)})
}

// ---------- status ----------

func (s *Server) handlerStatus(w http.ResponseWriter, _ *http.Request) {
	type acct struct {
		UID        string `json:"uid"`
		ExpiresIn  int64  `json:"expires_in"`
		HasRefresh bool   `json:"has_refresh"`
		HasAPIKey  bool   `json:"has_api_key"`
	}
	accs := make([]acct, 0, len(s.auths))
	for _, a := range s.auths {
		a.Lock()
		accs = append(accs, acct{
			UID:        a.UID,
			ExpiresIn:  a.ExpiresIn(),
			HasRefresh: a.RefreshToken != "",
			HasAPIKey:  a.APIKey != "",
		})
		a.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": accs,
		"models":   len(s.cfg.Models),
		"sessions": func() map[string]any {
			b, hits, misses, drops := s.router.Stats()
			return map[string]any{"bindings": b, "hits": hits, "misses": misses, "drops": drops}
		}(),
		"cache": map[string]any{
			"prompt_tokens":  atomic.LoadInt64(&s.statPrompt),
			"cache_read":     atomic.LoadInt64(&s.statCacheRead),
			"cache_creation": atomic.LoadInt64(&s.statCacheNew),
		},
	})
}

// ---------- models ----------

func (s *Server) handlerModels(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID            string `json:"id"`
		Object        string `json:"object"`
		OwnedBy       string `json:"owned_by"`
		ContextLength int    `json:"context_length,omitempty"`
		Upstream      string `json:"upstream,omitempty"`
	}
	seen := map[string]bool{}
	var data []item
	add := func(id, up string) {
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			return
		}
		seen[key] = true
		data = append(data, item{ID: id, Object: "model", OwnedBy: "phanthy",
			ContextLength: upstream.ContextLength(id), Upstream: up})
	}
	// 1) 配置里的别名（客户端遇到不认识的模型名会回落到 default_model，这层先暴露出去）
	for name, up := range s.cfg.Models {
		if up != "" && up != name {
			add(name, up)
		}
	}
	// 2) 配置里的直通模型
	for name, up := range s.cfg.Models {
		if up == "" || up == name {
			add(name, "")
		}
	}
	// 3) 客户端内置目录（上游允许列表在服务端，这里是候选全集，未必都能用）
	for name := range upstream.Catalog {
		add(name, "")
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---------- chat（OpenAI 兼容 → Phanthy /v1/messages）----------

// mapModel 别名 → 上游模型名。
// 未命中时**透传原名**：上游模型目录比客户端内置表更新（网页上已有 glm-5.3、
// Phana-5.5o 等），把未知名字静默改成 default_model 会让客户端以为换了模型、
// 实际还跑旧的那个。要禁止透传就开 upstream.strict_models。
func (s *Server) mapModel(name string) (string, bool) {
	if up, ok := s.cfg.Models[name]; ok && up != "" {
		return up, true
	}
	if name != "" {
		if s.cfg.Upstream.StrictModels {
			return "", false
		}
		return name, true
	}
	return s.cfg.Upstream.DefaultModel, true
}

func stopReasonToOpenAI(v any) string {
	switch s := fmt.Sprint(v); s {
	case "end_turn", "stop_sequence", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// ---------- Phanthy 只读代理 ----------

func (s *Server) handlerPhanthyCredits(w http.ResponseWriter, r *http.Request) {
	s.proxyPhanthy(w, r, "/api/oauth/usage/summary")
}

func (s *Server) handlerPhanthyProfile(w http.ResponseWriter, r *http.Request) {
	s.proxyPhanthy(w, r, "/api/oauth/profile")
}

// handlerPhanthyActivity 活动状态：走装机身份签名（未签名调用会被判 desktop_installation_required）。
func (s *Server) handlerPhanthyActivity(w http.ResponseWriter, r *http.Request) {
	a := s.pickAccount()
	if a == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "no_account"}})
		return
	}
	act, err := s.activities(a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	a.Lock()
	token := a.AccessToken
	a.Unlock()
	sum, st, raw, err := act.Summary(token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}, "upstream": string(raw)})
		return
	}
	writeJSON(w, st, sum)
}

func (s *Server) handlerPhanthyUsage(w http.ResponseWriter, r *http.Request) {
	s.proxyPhanthy(w, r, "/api/oauth/usage")
}

// handlerPhanthyRewards 领奖记录：同样要签名四头。
func (s *Server) handlerPhanthyRewards(w http.ResponseWriter, r *http.Request) {
	a := s.pickAccount()
	if a == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "no_account"}})
		return
	}
	act, err := s.activities(a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	a.Lock()
	token := a.AccessToken
	a.Unlock()
	st, raw, err := act.Rewards(token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_, _ = w.Write(raw)
}

func (s *Server) handlerPhanthyWorkRewards(w http.ResponseWriter, r *http.Request) {
	s.proxyPhanthy(w, r, "/api/work/rewards?limit=5")
}

func (s *Server) handlerPhanthyReferrals(w http.ResponseWriter, r *http.Request) {
	s.proxyPhanthy(w, r, "/api/oauth/referrals/me")
}

// proxyPhanthy 以首选账号的 token 请求 Phanthy 远端端点（GET 形式；写操作走代理另行）。
func (s *Server) proxyPhanthy(w http.ResponseWriter, _ *http.Request, path string) {
	a := s.pickAccount()
	if a == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "no_account"}})
		return
	}
	a.Lock()
	at := a.AccessToken
	a.Unlock()
	status, raw, err := s.client.Do(at, http.MethodGet, path, nil, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pickAccount 轮转首个可用账号。
func (s *Server) pickAccount() *auth.Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		return nil
	}
	// 简单轮转：取第一个未锁定过期的
	for _, a := range s.auths {
		a.Lock()
		dead := a.ExpiresIn() < -86400 // 已过期超 1 天视为不可用
		a.Unlock()
		if !dead {
			return a
		}
	}
	return s.auths[0]
}

// keepaliveLoop 主动续期：每 interval（默认 60s）检查一遍，剩余寿命 <= threshold 的 token 立刻续。
//
// 阈值必须显著大于 tick 间隔，否则两次 tick 之间就会出现"已过期但没人管"的窗口。
// 旧实现是 tick=1h、阈值=5min、token 寿命 30min：空闲账号必然在两次 tick 之间过期，
// 最长烂 55 分钟（2026-10-28 实测：expiresAt 过后 74s 无任何刷新，文件 mtime 不变）。
func (s *Server) keepaliveLoop() {
	interval := time.Duration(s.cfg.Schedule.KeepaliveIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	threshold := time.Duration(s.cfg.Schedule.KeepaliveThresholdSeconds) * time.Second
	if threshold <= 0 {
		threshold = 10 * time.Minute
	}
	log.Printf("[keepalive] 启动：每 %s 检查，剩余寿命 <= %s 即续期（阈值必须大于间隔）", interval, threshold)
	// 先扫一遍再进 ticker：旧实现第一次动手要等满一个 sleep 周期，服务起来后头一小时完全没人续期。
	s.keepaliveOnce(threshold)
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		s.keepaliveOnce(threshold)
	}
}

// keepaliveOnce 扫一遍账号池。
func (s *Server) keepaliveOnce(threshold time.Duration) {
	for _, a := range s.auths {
		s.refreshIfNeeded(a, threshold)
	}
}

// refreshIfNeeded 单飞续期：NeedsRefresh → 抢 RefreshMu → 二次确认 → 换 token → 落盘。
// 返回是否真的刷新了。refresh_token 每次刷新都会轮换，没有这把锁两个 goroutine 同时刷
// 会让其中一个拿到已作废的 refresh_token。
func (s *Server) refreshIfNeeded(a *auth.Auth, threshold time.Duration) bool {
	if !a.NeedsRefresh(threshold) {
		return false
	}
	a.RefreshMu.Lock()
	defer a.RefreshMu.Unlock()
	if !a.NeedsRefresh(threshold) { // 等锁期间别的 goroutine 已经刷过
		return false
	}
	a.Lock()
	rt, uid := a.RefreshToken, a.UID
	a.Unlock()
	if rt == "" {
		return false
	}
	tr, err := s.client.RefreshToken(rt)
	if err != nil {
		log.Printf("[refresh] %s 失败: %v", uid, err)
		return false
	}
	exp := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.ExpiresIn <= 0 {
		exp = time.Now().Add(time.Duration(upstream.DefaultExpires) * time.Second)
	}
	a.Lock()
	a.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		a.RefreshToken = tr.RefreshToken
	}
	a.ExpiresAt = exp.Unix()
	a.Unlock()
	if err := a.SaveAtomic(); err != nil {
		log.Printf("[refresh] %s 落盘失败: %v", uid, err)
	}
	log.Printf("[refresh] %s 已续期，剩余 %s", uid, time.Until(exp).Round(time.Second))
	return true
}
