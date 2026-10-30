// 活动/积分：装机身份装配、claim 编排、每日定时领取。
//
// 端点（本地路径 -> 线上上游路径）见 internal/upstream/activities.go 顶注。
// 这里只做三件事：拿到 accountScope、按账号缓存 Activities、在配置的小时点领一次。
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

// claimState 每账号当日领取状态（幂等由服务端保证，这里只防止重复跑）。
type claimState struct {
	mu   sync.Mutex
	last map[string]string // accountScope -> server_date
}

// AccountScope 取 /api/oauth/profile 的 account.uuid（桌面端 identity 的 accountScope）。
func (s *Server) accountScope(a *auth.Auth, token string) (string, error) {
	a.Lock()
	at := a.AccessToken
	a.Unlock()
	if at == "" {
		at = token
	}
	st, raw, err := s.client.Do(at, http.MethodGet, "/api/oauth/profile", nil, nil)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", fmt.Errorf("profile http %d: %s", st, string(raw))
	}
	var doc struct {
		Account struct {
			UUID string `json:"uuid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if doc.Account.UUID == "" {
		return "", fmt.Errorf("profile 缺少 account.uuid")
	}
	return doc.Account.UUID, nil
}

// activities 装配并缓存某账号的 Activities（身份文件落在 auth_dir）。
func (s *Server) activities(a *auth.Auth) (*upstream.Activities, error) {
	scope, err := s.accountScope(a, "")
	if err != nil {
		return nil, err
	}
	s.actsMu.Lock()
	defer s.actsMu.Unlock()
	if act, ok := s.acts[scope]; ok {
		return act, nil
	}
	id, err := s.pool.Identity(scope)
	if err != nil {
		return nil, err
	}
	act := upstream.NewActivities(s.client, id)
	s.acts[scope] = act
	return act, nil
}

// identityRegistered 本机这份装机身份是否已在服务端登记过。
// 判据：登记后会写回 serverInstallationId 与 status（active）。两者都有才算。
func identityRegistered(id *upstream.Identity) bool {
	if id == nil {
		return false
	}
	// 两个字段任意一个有值就算"服务端已知"：ServerID 来自成功的 register 响应，
	// status 来自 register 或"summary 成功"的旁证（见 summaryForTries）。
	// 只看 ServerID 会漏判：早期 register 常被上游 503 掉，但装机其实早就存在。
	return id.ServerID != "" || id.Status != ""
}

// ensureRegistered 保证「这个账号 + 这套装机身份」在服务端是 active 的。
//
// 为什么必须在这里做：上游 /activities/summary 要求 account 上存在一个活跃的
// desktop installation，否则一律 401 desktop_installation_required
// （"An active desktop installation for this account is required."）。
// 身份文件只有本机生成的三把公钥时，服务端根本没登记过 —— 以前只有 claimOne()
// 会 register，于是「新登录的号在看板上全是 0」：summary 401，面板用零值兜底。
//
// register 是幂等的（重复调用只更新 last_version），所以这里可以安全地兜底调用。
func (s *Server) ensureRegistered(a *auth.Auth, act *upstream.Activities, token string, tries int) error {
	if token == "" {
		return fmt.Errorf("无 accessToken")
	}
	if identityRegistered(act.Identity()) {
		return nil
	}
	// 失败冷却：register 一旦 503（活动服务降级时会持续一阵），不能让看板每次轮询都
	// 等好几秒。10 分钟内只试一次，失败就把上次的错直接返回。
	s.regMu.Lock()
	if at, ok := s.regTry[a.UID]; ok && time.Since(at) < 10*time.Minute {
		s.regMu.Unlock()
		return fmt.Errorf("register 冷却中（%s 前刚失败，10 分钟内不重试）", time.Since(at).Round(time.Second))
	}
	s.regMu.Unlock()

	if tries < 1 {
		tries = 1
	}
	// register 偶发 503 temporarily_unavailable（同一装机一天内多次注册更容易触发）。
	// 对新号来说这一炮必须打中：失败就等于 summary 401、面板全零。所以 5xx 会重试。
	var (
		reg *upstream.RegisterResp
		st  int
		raw []byte
		err error
	)
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second) // 1s, 2s
		}
		reg, st, raw, err = act.Register(token, "macos", "2.43.0")
		if err == nil {
			break
		}
		if st < 500 {
			break // 4xx 重试无意义
		}
		log.Printf("[register] %s 第 %d 次失败 http=%d，%ds 后重试", a.UID, attempt+1, st, attempt+1)
	}
	if err != nil {
		s.regMu.Lock()
		if s.regTry == nil {
			s.regTry = map[string]time.Time{}
		}
		s.regTry[a.UID] = time.Now()
		s.regMu.Unlock()
		if id := act.Identity(); id != nil {
			id.RecordError("register", err)
			_ = id.Save()
		}
		return fmt.Errorf("register(%d): %w %s", st, err, truncateStr(string(raw), 200))
	}
	s.regMu.Lock()
	delete(s.regTry, a.UID)
	s.regMu.Unlock()
	if id := act.Identity(); id != nil {
		id.RecordRegister(reg.Installation.ID, reg.Installation.Edition,
			reg.Installation.Platform, reg.Installation.LastVersion, reg.Installation.Status)
		if err := id.Save(); err != nil {
			log.Printf("[register] %s 身份写盘失败: %v", a.UID, err)
		}
	}
	log.Printf("[register] %s 装机身份已登记 serverInstallationId=%s status=%s",
		a.UID, reg.Installation.ID, reg.Installation.Status)
	return nil
}

// summaryFor 读摘要的标准入口：续期 → 确保登记 → summary；401 装机缺失时补登记再重试一次。
//
// 面板（collectOverview）、登录回显、claim 都走这里，避免又出现"某条路径忘了 register"。
func (s *Server) summaryFor(a *auth.Auth) (*upstream.SummaryResp, *upstream.Activities, string, error) {
	return s.summaryForTries(a, 1)
}

// summaryForTries 同 summaryFor，但可指定登记重试次数（登录/claim 用 3，看板轮询用 1）。
func (s *Server) summaryForTries(a *auth.Auth, tries int) (*upstream.SummaryResp, *upstream.Activities, string, error) {
	act, err := s.activities(a)
	if err != nil {
		return nil, nil, "", fmt.Errorf("装配身份: %w", err)
	}
	s.refreshIfNeeded(a, 2*time.Minute)
	a.Lock()
	token := a.AccessToken
	a.Unlock()
	if token == "" {
		return nil, act, "", fmt.Errorf("账号 %s 无 accessToken", a.UID)
	}
	if err := s.ensureRegistered(a, act, token, tries); err != nil {
		// 503 之类的失败不阻断：summary 可能仍然可用（服务端已存在这套身份）
		log.Printf("[summary] %s 登记检查失败，仍尝试 summary: %v", a.UID, err)
	}
	sum, st, raw, err := act.Summary(token)
	if err == nil {
		// summary 能通 = 服务端认这套装机。把 status 补上，避免后续每次轮询都去 register。
		if id := act.Identity(); id != nil && id.Status == "" {
			id.Status = "active"
			_ = id.Save()
		}
		return sum, act, token, nil
	}
	if st == http.StatusUnauthorized && strings.Contains(string(raw), "desktop_installation_required") {
		log.Printf("[summary] %s 服务端说没有活跃装机，强制重新 register 后重试", a.UID)
		if id := act.Identity(); id != nil {
			id.ServerID, id.Status = "", "" // 让 ensureRegistered 真的去登记
		}
		if rerr := s.ensureRegistered(a, act, token, tries); rerr != nil {
			return nil, act, token, fmt.Errorf("summary(%d): %w %s（补登记失败: %v）", st, err, truncateStr(string(raw), 200), rerr)
		}
		if sum2, st2, raw2, err2 := act.Summary(token); err2 == nil {
			return sum2, act, token, nil
		} else {
			return nil, act, token, fmt.Errorf("summary(%d): %w %s", st2, err2, truncateStr(string(raw2), 200))
		}
	}
	return nil, act, token, fmt.Errorf("summary(%d): %w %s", st, err, truncateStr(string(raw), 200))
}

// claimOne 对单账号跑 register -> summary -> (claim) 并返回摘要。
func (s *Server) claimOne(a *auth.Auth, dry bool) (*upstream.SummaryResp, *upstream.ClaimResp, error) {
	act, err := s.activities(a)
	if err != nil {
		return nil, nil, fmt.Errorf("装配身份: %w", err)
	}
	a.Lock()
	token := a.AccessToken
	a.Unlock()
	if token == "" {
		return nil, nil, fmt.Errorf("账号 %s 无 accessToken", a.UID)
	}
	if err := s.ensureRegistered(a, act, token, 3); err != nil {
		// register 偶发 503（身份已在服务端），不阻断领取
		log.Printf("[claim] %s register 失败，继续 summary: %v", a.UID, err)
	}
	sum, st, raw, err := act.Summary(token)
	if err != nil {
		return nil, nil, fmt.Errorf("summary(%d): %w %s", st, err, truncateStr(string(raw), 200))
	}
	if act.Identity() != nil {
		act.Identity().RecordSummary(sum.Credits.Available, sum.Onboarding.EarnedPoints)
	}
	// 积分整包按套餐到期日失效 → 取 plan.expires_at 供"快过期"权重用。
	var planExp int64
	if us, ust, _, uerr := act.UsageSummary(token); uerr == nil {
		planExp = us.PlanExpiresAt()
	} else {
		log.Printf("[claim] %s usage/summary(%d) 失败，跳过到期时间刷新: %v", a.UID, ust, uerr)
	}
	a.SetUsage(int64(sum.Credits.Available), planExp)
	if dry || !sum.FeatureFlags.DailyEnabled || sum.Daily.Status != "eligible_today" {
		if act.Identity() != nil {
			_ = act.Identity().Save()
		}
		return sum, nil, nil
	}
	cl, st, raw, err := act.Claim(token, sum.Daily.ServerDate)
	if err != nil {
		if act.Identity() != nil {
			act.Identity().RecordError("claim", err)
			_ = act.Identity().Save()
		}
		return sum, nil, fmt.Errorf("claim(%d): %w %s", st, err, truncateStr(string(raw), 200))
	}
	if act.Identity() != nil {
		act.Identity().RecordClaim(cl.ServerDate, cl.Status, cl.Points, cl.StreakDay)
		_ = act.Identity().Save()
	}
	return sum, cl, nil
}

// claimLoop 在配置的小时点给每个账号领一次（服务端幂等，重复跑不会多领）。
func (s *Server) claimLoop() {
	hours := map[int]bool{}
	for _, h := range s.cfg.Schedule.ClaimHours {
		hours[h] = true
	}
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for now := range tick.C {
		if !hours[now.Hour()] {
			continue
		}
		day := now.Format("2006-01-02")
		for _, a := range s.auths {
			if a.NeedsRefresh(5 * time.Minute) {
				s.refreshOne(a)
			}
			sum, cl, err := s.claimOne(a, false)
			if err != nil {
				log.Printf("[claim] %s 失败: %v", a.UID, err)
				continue
			}
			if jitter := s.cfg.Identity.ClaimJitterSeconds; jitter > 0 {
				time.Sleep(time.Duration(rand.Intn(jitter*1000)) * time.Millisecond)
			}
			if cl != nil {
				log.Printf("[claim] %s 领取成功 date=%s +%d 积分 streak=%d credits=%d",
					a.UID, cl.ServerDate, cl.Points, cl.StreakDay, sum.Credits.Available)
			} else {
				log.Printf("[claim] %s 今日无需领取 status=%s streak=%d credits=%d",
					a.UID, sum.Daily.Status, sum.Daily.StreakDay, sum.Credits.Available)
			}
			s.claimState.mu.Lock()
			if s.claimState.last == nil {
				s.claimState.last = map[string]string{}
			}
			s.claimState.last[a.UID] = day
			s.claimState.mu.Unlock()
		}
	}
}

// refreshOne 兼容旧调用点：本质是 refreshIfNeeded(5 分钟)。
func (s *Server) refreshOne(a *auth.Auth) { s.refreshIfNeeded(a, 5*time.Minute) }

// handlerPhanthyClaim 手动触发一次领取（POST /v1/phanthy/claim?dry=1 只看不领）。
func (s *Server) handlerPhanthyClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"code": "method_not_allowed"}})
		return
	}
	dry := r.URL.Query().Get("dry") == "1"
	type result struct {
		UID      string `json:"uid"`
		Scope    string `json:"scope,omitempty"`
		Status   string `json:"daily_status,omitempty"`
		Date     string `json:"server_date,omitempty"`
		Claimed  bool   `json:"claimed"`
		Points   int    `json:"points,omitempty"`
		Streak   int    `json:"streak_day,omitempty"`
		Credits  int    `json:"credits_available,omitempty"`
		NextAt   string `json:"next_eligible_at,omitempty"`
		ErrorMsg string `json:"error,omitempty"`
	}
	var out []result
	for _, a := range s.auths {
		res := result{UID: a.UID}
		if a.NeedsRefresh(5 * time.Minute) {
			s.refreshOne(a)
		}
		sum, cl, err := s.claimOne(a, dry)
		if err != nil {
			res.ErrorMsg = err.Error()
			out = append(out, res)
			continue
		}
		res.Status = sum.Daily.Status
		res.Date = sum.Daily.ServerDate
		res.Streak = sum.Daily.StreakDay
		res.Credits = sum.Credits.Available
		res.NextAt = sum.Daily.NextEligibleAt
		if cl != nil {
			res.Claimed = true
			res.Points = cl.Points
		}
		if act, err := s.activities(a); err == nil && act.Identity() != nil {
			res.Scope = act.Identity().AccountScope
		}
		out = append(out, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
