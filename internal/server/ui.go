// ui.go：内置看板（/ui/）与可视化登录页（/ui/login.html）的后端。
//
// 设计取舍：
//   - 纯静态前端（无构建步骤）：web/ 目录直接 go:embed，改样式不用重新编译前端工具链。
//   - 数据全部来自上游真实接口（activities summary + usage summary），不做本地推算：
//     积分数、套餐到期、签到状态、onboarding 进度都以服务端返回为准。
//   - 默认只允许本机访问（RemoteAddr 是回环地址）；要在别的机器上看就在 config 里设
//     ui.access_token，用 ?token= 或 X-UI-Token 头带上。网关的 api_key 对浏览器不友好，
//     所以这里单开一套鉴权，不复用 withAuth。
package server

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

//go:embed web
var webFS embed.FS

// ---------- 数据结构 ----------

type uiDaily struct {
	Status    string `json:"status"`
	Date      string `json:"server_date"`
	Streak    int    `json:"streak_day"`
	Points    int    `json:"points"`
	NextAt    string `json:"next_eligible_at"`
	NextPoint int    `json:"next_points"`
}

// uiPhoneEntry 手机号展示名的内存缓存（profile 不该被看板的轮询打爆）。
type uiPhoneEntry struct {
	label string
	at    time.Time
}

type uiOnboarding struct {
	Status  string `json:"status"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Earned  int    `json:"earned"`
	Max     int    `json:"max"`
	Pending int    `json:"pending"`
}

type uiReferral struct {
	AutoDone  int  `json:"auto_tiers_done"`
	NextScore int  `json:"next_reward_points"`
	Eligible  bool `json:"eligible_to_invite"`
	Total     int  `json:"total_granted"`
}

type uiAccount struct {
	UID           string       `json:"uid"`
	Label         string       `json:"label"`
	LabelFull     string       `json:"label_full"`
	Credits       int64        `json:"credits"`
	PlanName      string       `json:"plan_name"`
	PlanExpiresAt string       `json:"plan_expires_at"`
	PlanLeftHours float64      `json:"plan_left_hours"`
	TokenLeftSec  int64        `json:"token_left_seconds"`
	Weight        float64      `json:"weight"`
	CoolingSec    int64        `json:"cooling_seconds"`
	LastUsedAgo   int64        `json:"last_used_ago_seconds"`
	Daily         uiDaily      `json:"daily"`
	Onboarding    uiOnboarding `json:"onboarding"`
	Referral      uiReferral   `json:"referral"`
	Error         string       `json:"error,omitempty"`
}

type uiOverview struct {
	GeneratedAt string `json:"generated_at"`
	Server      struct {
		Base      string `json:"base"`
		Listen    string `json:"listen"`
		Accounts  int    `json:"accounts"`
		Sticky    int    `json:"sticky_sessions"`
		UptimeSec int64  `json:"uptime_seconds"`
	} `json:"server"`
	Totals struct {
		Credits        int64   `json:"credits"`
		Accounts       int     `json:"accounts"`
		ClaimedToday   int     `json:"claimed_today"`
		OnboardingDone int     `json:"onboarding_done"`
		OnboardingMax  int     `json:"onboarding_total"`
		OnboardingPts  int     `json:"onboarding_points"`
		ReferralAuto   int     `json:"referral_auto_tiers"`
		NearestExpiry  string  `json:"nearest_plan_expiry"`
		NearestLeftHrs float64 `json:"nearest_plan_hours"`
	} `json:"totals"`
	Accounts []uiAccount `json:"accounts"`
	Jobs     []jobStatus `json:"jobs"`
	Invite   *uiInvite   `json:"invite,omitempty"`
}

// uiInvite 是「带码注册」链接：邀请关系只在注册表单提交那一刻建立，
// 已建好的账号事后 bind 永久 409，所以这里给的是注册入口而不是补填按钮。
type uiInvite struct {
	At         string `json:"at"`
	Inviter    string `json:"inviter"`
	Phone      string `json:"phone,omitempty"`
	Tier       int    `json:"tier"`
	Budget     int    `json:"budget"`
	NextReward int    `json:"next_reward"`
	InviteCode string `json:"invite_code"`
	ShareURL   string `json:"share_url"`
	Note       string `json:"note"`
}

// readInviteCandidate 读 scripts/invite_link.py 产出的 data/invite_candidate.json，
// 兼容早期 invite_flow.py 的驼峰 schema。
func readInviteCandidate() *uiInvite {
	b, err := os.ReadFile("data/invite_candidate.json")
	if err != nil {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := raw[k].(float64); ok {
				return int(v)
			}
		}
		return 0
	}
	inv := &uiInvite{
		At:         str("at", "createdAt"),
		Inviter:    str("inviter"),
		Phone:      str("phone"),
		Tier:       num("tier", "inviterTier"),
		Budget:     num("budget", "inviterBudgetLeft"),
		NextReward: num("next_reward", "inviterNextReward"),
		InviteCode: str("invite_code", "inviteCode"),
		ShareURL:   str("share_url", "shareUrl"),
	}
	if inv.Budget == 0 && inv.Tier < 2 {
		inv.Budget = 2 - inv.Tier
	}
	inv.Note = str("note", "hint")
	if inv.Note == "" {
		inv.Note = "关系只在注册表单建立；已建好的号无法补填"
	}
	if inv.ShareURL == "" {
		return nil
	}
	return inv
}

type uiUsageDay struct {
	Date     string `json:"date"`
	Points   int64  `json:"points"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

type uiUsageModel struct {
	Model    string  `json:"model"`
	Points   int64   `json:"points"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Share    float64 `json:"share"`
}

type uiUsage struct {
	Days      int              `json:"days"`
	Series    []uiUsageDay     `json:"series"`
	ByModel   []uiUsageModel   `json:"by_model"`
	ByAccount []map[string]any `json:"by_account"`
	Totals    struct {
		Points   int64 `json:"points"`
		Requests int64 `json:"requests"`
		Tokens   int64 `json:"tokens"`
	} `json:"totals"`
}

// ---------- 缓存 ----------

type uiCacheEntry[T any] struct {
	at  time.Time
	val T
}

// ---------- 路由 ----------

func (s *Server) mountUI(mux *http.ServeMux) {
	if !s.cfg.UI.Enabled {
		return
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return
	}
	files := http.StripPrefix("/ui", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	// "/ui/" 前缀必须带尾斜杠，否则 ServeMux 只匹配精确路径（/ui/app.css 会 404）。
	mux.Handle("/ui/", s.uiGuardHandler(files))
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/login.html", http.StatusFound)
	})
	mux.HandleFunc("/api/ui/overview", s.uiGuard(s.handlerUIOverview))
	mux.HandleFunc("/api/ui/usage", s.uiGuard(s.handlerUIUsage))
	mux.HandleFunc("/api/ui/claim", s.uiGuard(s.handlerUIClaim))
	mux.HandleFunc("/api/ui/refresh", s.uiGuard(s.handlerUIRefresh))
	mux.HandleFunc("/api/ui/run", s.uiGuard(s.handlerUIRun))
	mux.HandleFunc("/api/ui/login/start", s.uiGuard(s.handlerUILoginStart))
	mux.HandleFunc("/api/ui/login/exchange", s.uiGuard(s.handlerUILoginExchange))
}

// uiAllowed 本机直连放行；配了 ui.access_token 时非本机也可带 token 访问。
func (s *Server) uiAllowed(r *http.Request) bool {
	if tok := s.cfg.UI.AccessToken; tok != "" {
		if r.URL.Query().Get("token") == tok || r.Header.Get("X-UI-Token") == tok {
			return true
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

// uiGuardHandler 与 uiGuard 同义，只是给 http.Handler 用（静态文件）。
func (s *Server) uiGuardHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.uiAllowed(r) {
			http.Error(w, "ui: unauthorized（默认只允许本机访问，或配置 ui.access_token）", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) uiGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.uiAllowed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "ui_unauthorized"}})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// ---------- 总览 ----------

func (s *Server) handlerUIOverview(w http.ResponseWriter, r *http.Request) {
	ov := s.collectOverview(true)
	writeJSON(w, http.StatusOK, ov)
}

func (s *Server) collectOverview(hitUpstream bool) uiOverview {
	s.uiMu.Lock()
	if !hitUpstream && time.Since(s.uiOverviewAt) < 20*time.Second && s.uiOverview != nil {
		ov := *s.uiOverview
		s.uiMu.Unlock()
		return ov
	}
	s.uiMu.Unlock()

	var ov uiOverview
	ov.GeneratedAt = time.Now().Format(time.RFC3339)
	ov.Server.Base = s.cfg.BaseURL
	ov.Server.Listen = s.cfg.Listen
	ov.Server.UptimeSec = int64(time.Since(s.startedAt).Seconds())
	if n, _, _, _ := s.router.Stats(); true {
		ov.Server.Sticky = n
	}
	now := time.Now()
	maxCredits := maxCreditsOf(s.auths)
	var nearest time.Time
	for _, a := range s.auths {
		acct := uiAccount{UID: a.UID, Label: maskUID(a.UID), LabelFull: s.displayName(a)}
		if acct.LabelFull == "" {
			acct.LabelFull = a.UID
		}
		credits, planExp, lastUsed := a.UsageSnapshot()
		acct.Credits = credits
		acct.LastUsedAgo = 0
		if lastUsed > 0 {
			acct.LastUsedAgo = int64(now.Sub(time.Unix(lastUsed, 0)).Seconds())
		}
		a.Lock()
		acct.TokenLeftSec = a.ExpiresAt - now.Unix()
		token := a.AccessToken
		a.Unlock()
		acct.Weight = s.weightOf(a, maxCredits, now)
		s.mu.Lock()
		if until, ok := s.cooldown[a.UID]; ok && until.After(now) {
			acct.CoolingSec = int64(until.Sub(now).Seconds())
		}
		s.mu.Unlock()
		if planExp > 0 {
			t := time.Unix(planExp, 0)
			acct.PlanExpiresAt = t.Format(time.RFC3339)
			acct.PlanLeftHours = t.Sub(now).Hours()
			if nearest.IsZero() || t.Before(nearest) {
				nearest = t
			}
		}
		if hitUpstream && token != "" {
			// 走 summaryFor：先确认装机身份已在服务端登记（否则新号必然 401
			// desktop_installation_required），成功后再取 usage/referral。
			if sum, act, _, err := s.summaryFor(a); err == nil {
				acct.Credits = int64(sum.Credits.Available)
				acct.Daily = uiDaily{
					Status: sum.Daily.Status, Date: sum.Daily.ServerDate, Streak: sum.Daily.StreakDay,
					Points: sum.Daily.Points, NextAt: sum.Daily.NextEligibleAt, NextPoint: sum.Daily.NextPoints,
				}
				acct.Onboarding = uiOnboarding{
					Status: sum.Onboarding.Status, Done: sum.Onboarding.CompletedCount, Total: sum.Onboarding.TotalCount,
					Earned: sum.Onboarding.EarnedPoints, Max: sum.Onboarding.MaxPoints,
				}
				for _, t := range sum.Onboarding.Tasks {
					if t.Status != "completed" && t.Status != "granted" {
						acct.Onboarding.Pending++
					}
				}
				a.SetUsage(int64(sum.Credits.Available), planExp)
				if us, _, _, err := act.UsageSummary(token); err == nil && us.Plan.ExpiresAt != "" {
					if t, err := time.Parse(time.RFC3339, us.Plan.ExpiresAt); err == nil {
						acct.PlanExpiresAt = t.Format(time.RFC3339)
						acct.PlanLeftHours = t.Sub(now).Hours()
						if nearest.IsZero() || t.Before(nearest) {
							nearest = t
						}
					}
					if us.Plan.Name != "" {
						acct.PlanName = us.Plan.Name
					}
				}
				if me, _, _, err := act.ReferralMe(token); err == nil {
					acct.Referral = uiReferral{
						AutoDone: me.AutoTiersDone(), NextScore: me.Referral.NextRewardPoints,
						Eligible: me.Referral.EligibleToInvite, Total: me.Referral.TotalGranted,
					}
				}
			} else {
				acct.Error = errSummary(err)
			}
		}
		ov.Accounts = append(ov.Accounts, acct)
		ov.Totals.Credits += acct.Credits
		ov.Totals.OnboardingDone += acct.Onboarding.Done
		ov.Totals.OnboardingMax += acct.Onboarding.Total
		ov.Totals.OnboardingPts += acct.Onboarding.Earned
		ov.Totals.ReferralAuto += acct.Referral.AutoDone
		if acct.Daily.Status == "granted_today" || acct.Daily.Status == "already_granted" {
			ov.Totals.ClaimedToday++
		}
	}
	sort.Slice(ov.Accounts, func(i, j int) bool { return ov.Accounts[i].Credits > ov.Accounts[j].Credits })
	ov.Totals.Accounts = len(ov.Accounts)
	ov.Server.Accounts = len(ov.Accounts)
	if !nearest.IsZero() {
		ov.Totals.NearestExpiry = nearest.Format(time.RFC3339)
		ov.Totals.NearestLeftHrs = nearest.Sub(now).Hours()
	}
	if s.jobs != nil {
		ov.Jobs = s.jobs.list()
	}
	ov.Invite = readInviteCandidate()
	s.uiMu.Lock()
	s.uiOverview = &ov
	s.uiOverviewAt = time.Now()
	s.uiMu.Unlock()
	return ov
}

// displayName 账号展示名：优先手机号掩码（138****0000）。
//
// 为什么要惰性回填：昵称是登录时写进 auths 文件的，老版本存的可能是 "phanthy-1028"
// 或干脆没有；池子从别处拷过来也可能缺。这里在渲染时兜一次 profile，
// 成功就顺手写回文件（cmd/activity 也会回填，两条路殊途同归），带 30 分钟内存缓存。
func (s *Server) displayName(a *auth.Auth) string {
	if looksLikePhone(a.Nickname) {
		return a.Nickname
	}
	s.uiMu.Lock()
	if e, ok := s.uiPhone[a.UID]; ok && time.Since(e.at) < 30*time.Minute {
		s.uiMu.Unlock()
		if e.label != "" {
			return e.label
		}
		return a.Nickname
	}
	s.uiMu.Unlock()

	label := ""
	if a.NeedsRefresh(2 * time.Minute) {
		s.refreshIfNeeded(a, 2*time.Minute)
	}
	if prof, err := s.client.Profile(a.AccessToken); err == nil {
		label = prof.Label()
	}
	s.uiMu.Lock()
	if s.uiPhone == nil {
		s.uiPhone = map[string]uiPhoneEntry{}
	}
	s.uiPhone[a.UID] = uiPhoneEntry{label: label, at: time.Now()}
	s.uiMu.Unlock()

	if label != "" && label != a.Nickname {
		_ = a.SetNickname(label) // 失败不影响展示，下次还会再试
		return label
	}
	if a.Nickname != "" {
		return a.Nickname
	}
	return a.UID
}

// looksLikePhone 判断是否已经是手机号掩码（138****8888 这种）。
func looksLikePhone(s string) bool {
	return strings.Contains(s, "****") && len(s) >= 11
}

func errSummary(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// maskUID 只露尾 4 位：看板可能投屏/截图，uid 不是密钥但也别整串贴出去。
func maskUID(uid string) string {
	if len(uid) <= 4 {
		return uid
	}
	return "…" + uid[len(uid)-4:]
}

// ---------- 用量 ----------

func (s *Server) handlerUIUsage(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		switch v {
		case "7":
			days = 7
		case "14":
			days = 14
		case "30":
			days = 30
		}
	}
	cut := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	var out uiUsage
	out.Days = days
	byDate := map[string]*uiUsageDay{}
	byModel := map[string]*uiUsageModel{}
	for _, a := range s.auths {
		a.Lock()
		token := a.AccessToken
		a.Unlock()
		if token == "" {
			continue
		}
		act, err := s.activities(a)
		if err != nil {
			continue
		}
		us, _, _, err := act.UsageSummary(token)
		if err != nil || us == nil {
			continue
		}
		var accPoints, accReq int64
		for _, row := range us.UsageByDayAndModel {
			if row.Date < cut {
				continue
			}
			d := byDate[row.Date]
			if d == nil {
				d = &uiUsageDay{Date: row.Date}
				byDate[row.Date] = d
			}
			d.Points += row.CostPoints
			d.Requests += row.RequestCount
			d.Tokens += row.TotalTokens

			key := row.Model
			if key == "" {
				key = "(unknown)"
			}
			m := byModel[key]
			if m == nil {
				m = &uiUsageModel{Model: key}
				byModel[key] = m
			}
			m.Points += row.CostPoints
			m.Requests += row.RequestCount
			m.Tokens += row.TotalTokens

			accPoints += row.CostPoints
			accReq += row.RequestCount
			out.Totals.Points += row.CostPoints
			out.Totals.Requests += row.RequestCount
			out.Totals.Tokens += row.TotalTokens
		}
		out.ByAccount = append(out.ByAccount, map[string]any{"uid": maskUID(a.UID), "points": accPoints, "requests": accReq})
	}
	// 补齐没有用量的空白天：柱状图不能因为没调用就塌成一根，
	// 而且"整段没用量"本身就是信息（比如 token 全过期那天）。
	start, _ := time.ParseInLocation("2006-01-02", cut, time.Local)
	for d := start; !d.After(time.Now()); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if v, ok := byDate[key]; ok {
			out.Series = append(out.Series, *v)
		} else {
			out.Series = append(out.Series, uiUsageDay{Date: key})
		}
	}
	for _, m := range byModel {
		if out.Totals.Points > 0 {
			m.Share = float64(m.Points) / float64(out.Totals.Points)
		}
		out.ByModel = append(out.ByModel, *m)
	}
	sort.Slice(out.ByModel, func(i, j int) bool { return out.ByModel[i].Points > out.ByModel[j].Points })
	if out.Series == nil {
		out.Series = []uiUsageDay{}
	}
	if out.ByModel == nil {
		out.ByModel = []uiUsageModel{}
	}
	if out.ByAccount == nil {
		out.ByAccount = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- 动作：签到 / 续期 ----------

func (s *Server) handlerUIClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"code": "method_not_allowed"}})
		return
	}
	type row struct {
		UID     string `json:"uid"`
		Status  string `json:"status"`
		Points  int    `json:"points"`
		Streak  int    `json:"streak_day"`
		Credits int    `json:"credits"`
		Error   string `json:"error,omitempty"`
	}
	var rows []row
	for _, a := range s.auths {
		s.refreshIfNeeded(a, 5*time.Minute)
		sum, cl, err := s.claimOne(a, false)
		res := row{UID: maskUID(a.UID)}
		if err != nil {
			res.Error = err.Error()
			rows = append(rows, res)
			continue
		}
		if cl != nil {
			res.Status = cl.Status
			res.Points = cl.Points
		} else {
			res.Status = sum.Daily.Status
		}
		res.Streak = sum.Daily.StreakDay
		res.Credits = sum.Credits.Available
		rows = append(rows, res)
	}
	s.uiMu.Lock()
	s.uiOverviewAt = time.Time{}
	s.uiMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Server) handlerUIRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"code": "method_not_allowed"}})
		return
	}
	type row struct {
		UID       string `json:"uid"`
		Refreshed bool   `json:"refreshed"`
		LeftSec   int64  `json:"token_left_seconds"`
	}
	var rows []row
	for _, a := range s.auths {
		// 强制续期：把阈值设成「剩余寿命 + 1h」，等价于无条件刷一次
		a.Lock()
		left := time.Duration(a.ExpiresAt-time.Now().Unix()) * time.Second
		a.Unlock()
		ok := s.refreshIfNeeded(a, left+time.Hour)
		a.Lock()
		rows = append(rows, row{UID: maskUID(a.UID), Refreshed: ok, LeftSec: a.ExpiresAt - time.Now().Unix()})
		a.Unlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

// ---------- 可视化登录（PKCE 两步）----------

type uiPendingLogin struct {
	verifier string
	at       time.Time
}

func (s *Server) handlerUILoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"code": "method_not_allowed"}})
		return
	}
	v, ch, err := upstream.NewVerifier()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	id := randID()
	s.uiMu.Lock()
	if s.uiPending == nil {
		s.uiPending = map[string]uiPendingLogin{}
	}
	// 顺手清掉 10 分钟前的 pending，避免内存里堆死数据
	for k, p := range s.uiPending {
		if time.Since(p.at) > 10*time.Minute {
			delete(s.uiPending, k)
		}
	}
	s.uiPending[id] = uiPendingLogin{verifier: v, at: time.Now()}
	s.uiMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "url": s.client.AuthorizeURL(ch)})
}

func (s *Server) handlerUILoginExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"code": "method_not_allowed"}})
		return
	}
	var in struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "bad json"}})
		return
	}
	s.uiMu.Lock()
	p, ok := s.uiPending[in.ID]
	s.uiMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "unknown_login_id", "message": "登录会话不存在或已过期，请重新生成授权链接"}})
		return
	}
	code := cleanCode(in.Code)
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "empty_code", "message": "没读到授权码"}})
		return
	}
	tr, err := s.client.ExchangeCode(code, p.verifier)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"code": "exchange_failed", "message": err.Error()}})
		return
	}
	// 身份判定与 cmd/login 一致：accountScope 去重，昵称用手机号掩码。
	scope, label := auth.JWTSub(tr.AccessToken), ""
	if prof, perr := s.client.Profile(tr.AccessToken); perr == nil {
		if prof.Account.UUID != "" {
			scope = prof.Account.UUID
		}
		label = prof.Label()
	}
	if label == "" {
		label = "phanthy-" + time.Now().Format("0102")
	}
	path, deduped, err := auth.SaveLogin(s.cfg.AuthDir, scope, tr.AccessToken, tr.RefreshToken,
		tr.TokenType, tr.IDToken, label, time.Now().Add(time.Duration(expiresOr(tr.ExpiresIn))*time.Second).Unix())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"message": err.Error()}})
		return
	}
	s.uiMu.Lock()
	delete(s.uiPending, in.ID)
	s.uiOverviewAt = time.Time{}
	s.uiMu.Unlock()

	// 立刻用新凭证拉一次摘要，页面上直接显示这个号有多少积分
	out := map[string]any{"ok": true, "file": path, "label": label, "scope": scope, "merged": deduped}
	if a, err := auth.LoadAuthFile(path); err == nil {
		s.addAccount(a)
		// 新号登录后立刻登记装机身份并读摘要：不能等 09:00 的 claim job，
		// 否则登录后到第一次 claim 之间，面板上这个号全是 0（summary 401）。
		if sum, _, _, err := s.summaryForTries(a, 3); err == nil {
			out["credits"] = sum.Credits.Available
			out["onboarding"] = map[string]any{"done": sum.Onboarding.CompletedCount, "total": sum.Onboarding.TotalCount}
			out["registered"] = true
		} else {
			out["registered"] = false
			out["register_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handlerUIRun 面板上的「立即运行」：异步触发一条定时任务，立刻返回，前端轮询 overview 看结果。
func (s *Server) handlerUIRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "POST only"}})
		return
	}
	var body struct {
		Job string `json:"job"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.jobs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "没有配置定时任务（config.schedule.jobs 为空）"}})
		return
	}
	name := strings.TrimSpace(body.Job)
	known := false
	for _, j := range s.jobs.cfg {
		if j.Name == name {
			known = true
		}
	}
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "未知任务: " + name}})
		return
	}
	go func() {
		if rc, tail, err := s.jobs.run(name); err != nil {
			log.Printf("[job] 手动触发 %s 失败 rc=%d: %v", name, rc, err)
		} else {
			log.Printf("[job] 手动触发 %s 完成 rc=%d\n%s", name, rc, tail)
		}
	}()
	s.uiMu.Lock()
	s.uiOverviewAt = time.Time{}
	s.uiMu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job": name, "accepted": true})
}

func expiresOr(v int64) int64 {
	if v <= 0 {
		return upstream.DefaultExpires
	}
	return v
}

// addAccount 把刚登录的账号塞进运行池（已存在则替换）。
func (s *Server) addAccount(a *auth.Auth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, old := range s.auths {
		if old.UID == a.UID {
			s.auths[i] = a
			return
		}
	}
	s.auths = append(s.auths, a)
}

// cleanCode 用户可能整段粘贴成功页 URL 或 code#state，统一剥出 code。
func cleanCode(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "code="); i >= 0 {
		s = s[i+5:]
		if j := strings.IndexAny(s, "& \n"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

// randID 16 字节随机 hex，用作一次登录会话的 id（只在本进程内存里活着）。
func randID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b[:])
}
