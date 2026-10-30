// 账号池运维 CLI：巡检 / 注册 / 领取 / 对账 / 推理自测。
//
//	go run ./cmd/activity -action status     -auths ./auths
//	go run ./cmd/activity -action provision  -auths ./auths   # 给每个账号装/补一台虚拟桌面
//	go run ./cmd/activity -action claim      -auths ./auths   # 只领今天该领的
//	go run ./cmd/activity -action reconcile  -auths ./auths   # 让服务端按真实用量结算 onboarding
//	go run ./cmd/activity -action probe      -auths ./auths   # 发一条最小真实推理，验证证明链
//
// 设计要点：
//   - 每个账号一个装机身份（installation-<accountScope>.json），device 钥匙作用域可切；
//   - 账号之间串行 + 抖动（-delay），避免同一出口 IP 上瞬时打出一串注册/领取；
//   - 单账号失败只记到该身份的 lastError，不中断整池；
//   - 所有状态写回身份文件，进程重启后无需重新注册即可继续。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

type row struct {
	UID           string `json:"uid"`
	Nickname      string `json:"nickname,omitempty"`
	Scope         string `json:"accountScope"`
	InstallID     string `json:"installationId"`
	ServerID      string `json:"serverInstallationId,omitempty"`
	Status        string `json:"status,omitempty"`
	DeviceScope   string `json:"deviceScope,omitempty"`
	DailyStatus   string `json:"dailyStatus,omitempty"`
	Streak        int    `json:"streakDay,omitempty"`
	ClaimedTotal  int    `json:"lastClaimPoints,omitempty"`
	ClaimDate     string `json:"lastClaimDate,omitempty"`
	Credits       int    `json:"creditsAvailable,omitempty"`
	Onboarding    int    `json:"onboardingEarned,omitempty"`
	OnboardingMax int    `json:"onboardingMax,omitempty"`
	NextEligible  string `json:"nextEligibleAt,omitempty"`
	// 邀请（referral）观测值
	InviteCode     string `json:"inviteCode,omitempty"`
	InviteTier     int    `json:"referralTierGranted,omitempty"` // 已自动到账档位数
	InviteBudget   int    `json:"inviteBudgetLeft,omitempty"`    // 前两档还剩几次
	InviteNext     int    `json:"referralNextPoints,omitempty"`  // 下一档自动奖励
	InviteTotal    int    `json:"referralTotalPoints,omitempty"`
	InviteEligible *bool  `json:"referralEligible,omitempty"`
	Note           string `json:"note,omitempty"`
	Error          string `json:"error,omitempty"`
}

func main() {
	action := flag.String("action", "status", "status|provision|claim|reconcile|probe|rotate|referral|referral-code|referral-pick|referral-relations|referral-bind(已废弃)")
	authDir := flag.String("auths", "auths", "凭证目录（同时存放装机身份）")
	base := flag.String("base", "https://code.phanthy.com", "上游基址")
	deviceScope := flag.String("device-scope", "account", "account|machine：设备钥匙是否全池共用")
	asJSON := flag.Bool("json", false, "输出 JSON")
	delay := flag.Duration("delay", 2*time.Second, "账号之间的基础间隔（会叠加 0~delay 抖动）")
	model := flag.String("model", "phanthy-pro", "probe 用模型；套餐没开通的模型会返回 model_not_allowed")
	prompt := flag.String("prompt", "用一句话说明你能做什么。", "probe 用提示词")
	rotate := flag.String("account", "", "rotate 用：要换机的 accountScope 或 uid；referral-pick 用：强制指定邀请人")
	state := flag.String("state", "", "referral-bind 用：/r/<code> 页面里的 referral_state（rs1.…）。已废弃：已建账号永久 409")
	cursor := flag.String("cursor", "", "referral-relations 用：翻页游标")
	limit := flag.Int("limit", 20, "referral-relations 用：每页条数")
	flag.Parse()

	auths, err := auth.LoadAuths(*authDir)
	if err != nil {
		fatal("读取凭证目录 %s: %v", *authDir, err)
	}
	// 过滤：装机身份文件不是账号凭证
	var accounts []*auth.Auth
	for _, a := range auths {
		if strings.HasPrefix(filepath.Base(a.FilePath), "installation-") || filepath.Base(a.FilePath) == upstream.MachineDeviceFile {
			continue
		}
		accounts = append(accounts, a)
	}
	if len(accounts) == 0 {
		fatal("%s 里没有账号凭证", *authDir)
	}

	cli := upstream.New(*base)
	cli.HTTP.Timeout = 60 * time.Second
	pool := upstream.NewPool(*authDir, *deviceScope)

	if *action == "rotate" {
		if *rotate == "" {
			fatal("rotate 需要 -account <accountScope|uid>")
		}
		scope, err := rotateIdentity(pool, *rotate, accounts)
		if err != nil {
			fatal("rotate: %v", err)
		}
		fmt.Printf("已换机：accountScope=%s 旧身份备份见 auths/installation-%s.bak-*.json\n", scope, scope)
		rows := make([]row, 0, len(accounts))
		for _, a := range accounts {
			rows = append(rows, runAccount(cli, pool, a, runOpts{action: "provision", model: *model, prompt: *prompt}))
		}
		printTable(rows)
		return
	}

	// referral-pick：先逐账号刷新 referrals/me（权威档位），再随机挑一个「前两档未满」的邀请人，
	// 并确保它已有专属邀请码。账号池按「一个邀请人只做前两次自动奖励」调度。
	if *action == "referral-pick" {
		opt := runOpts{action: "referral"}
		var scopes []string
		byScope := map[string]*auth.Auth{}
		failed := 0
		for i, a := range accounts {
			if i > 0 {
				sleepJitter(*delay)
			}
			r := runAccount(cli, pool, a, opt)
			if r.Scope != "" {
				scopes = append(scopes, r.Scope)
				byScope[r.Scope] = a
			}
			if r.Error != "" && !strings.HasPrefix(r.Error, "register http 503") {
				failed++
				fmt.Fprintf(os.Stderr, "[referral-pick] %s: %s\n", trunc(r.UID, 26), r.Error)
			}
		}
		ids := make([]*upstream.Identity, 0, len(scopes))
		for _, s := range scopes {
			if id, err := pool.Identity(s); err == nil {
				ids = append(ids, id)
			}
		}
		var picked *upstream.Identity
		if *rotate != "" { // 指定邀请人（例如固定让老号拿前两档）
			for _, id := range ids {
				if id.AccountScope == *rotate {
					picked = id
				}
			}
			if picked == nil {
				fmt.Fprintf(os.Stderr, "referral-pick: 找不到 accountScope=%s\n", *rotate)
				os.Exit(1)
			}
			if !picked.CanInvite() {
				fmt.Fprintf(os.Stderr, "referral-pick: %s 前两档已满或不可邀请（tier=%d）\n", *rotate, picked.ReferralTierGranted)
				os.Exit(1)
			}
		} else {
			picked = upstream.PickInviter(ids, rand.Intn)
		}
		if picked == nil {
			fmt.Println("没有可用邀请人：全池账号的前两档（5,000 / 8,000）都已到账或不可邀请")
			os.Exit(1)
		}
		// 保证有码
		if picked.ReferralShareURL == "" || picked.ReferralInviteCode == "" {
			if a := byScope[picked.AccountScope]; a != nil {
				r := runAccount(cli, pool, a, runOpts{action: "referral-code"})
				if r.Error != "" {
					fmt.Fprintf(os.Stderr, "[referral-pick] 生成邀请码失败 %s: %s\n", picked.AccountScope, r.Error)
				}
				picked, _ = pool.Identity(picked.AccountScope)
			}
		}
		out := map[string]any{
			"accountScope":       picked.AccountScope,
			"inviteCode":         picked.ReferralInviteCode,
			"shareUrl":           picked.ReferralShareURL,
			"tierGranted":        picked.ReferralTierGranted,
			"budgetLeft":         picked.InviteBudgetLeft(),
			"nextRewardPoints":   picked.ReferralNextPoints,
			"totalEarned":        picked.ReferralTotalPoints,
			"eligible":           picked.ReferralEligible,
			"referralCheckedAt":  picked.ReferralCheckedAt,
			"poolFailedAccounts": failed,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		if picked.ReferralShareURL == "" {
			os.Exit(1)
		}
		return
	}

	rows := make([]row, 0, len(accounts))
	opt := runOpts{action: *action, model: *model, prompt: *prompt, state: *state, cursor: *cursor, limit: *limit}
	for i, a := range accounts {
		if i > 0 {
			sleepJitter(*delay)
		}
		rows = append(rows, runAccount(cli, pool, a, opt))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
	} else {
		printTable(rows)
	}
	// 有失败就以非零退出，便于 cron/CI 发现
	for _, r := range rows {
		// register 503 是服务端临时拒绝（身份已在），不算整池失败
		if r.Error != "" && !strings.HasPrefix(r.Error, "register http 503") {
			os.Exit(1)
		}
	}
}

// runOpts 一次动作的参数集（避免 runAccount 参数列表无限膨胀）。
type runOpts struct {
	action string
	model  string
	prompt string
	state  string // referral-bind
	cursor string // referral-relations
	limit  int
}

// runAccount 单账号一条龙：刷新 → 取 scope → 装身份 → register → 按 action 动作。
func runAccount(cli *upstream.Client, pool *upstream.Pool, a *auth.Auth, opt runOpts) row {
	action, model, prompt := opt.action, opt.model, opt.prompt
	r := row{UID: a.UID}
	fail := func(step string, err error) row {
		r.Error = step + ": " + short(err.Error())
		if id, e := pool.Identity(r.Scope); e == nil && id != nil {
			id.RecordError(step, err)
			_ = id.Save()
		}
		return r
	}

	// 1) token
	if a.NeedsRefresh(5 * time.Minute) {
		resp, err := cli.RefreshToken(a.RefreshToken)
		if err != nil {
			return fail("refresh", err)
		}
		a.Lock()
		a.AccessToken = resp.AccessToken
		if resp.RefreshToken != "" {
			a.RefreshToken = resp.RefreshToken
		}
		if resp.ExpiresIn > 0 {
			a.ExpiresAt = time.Now().Unix() + resp.ExpiresIn
		}
		a.Unlock()
		if err := a.SaveAtomic(); err != nil {
			return fail("save-token", err)
		}
	}
	token := a.AccessToken

	// 2) accountScope（桌面身份的 keyring 账号名）
	prof, err := cli.Profile(token)
	if err != nil {
		return fail("profile", err)
	}
	if prof.Account.UUID == "" {
		return fail("profile", fmt.Errorf("缺少 account.uuid"))
	}
	r.Scope = prof.Account.UUID
	// 展示名回填：老文件里存的是登录时的 "phanthy-1028"，换成手机号掩码
	// （138****0000）。只在变化时落盘，避免每条记录都写文件。
	if lb := prof.Label(); lb != "" && lb != a.Nickname {
		if err := a.SetNickname(lb); err == nil {
			r.Nickname = lb
		}
	}
	if a.UID == "" {
		r.UID = r.Scope
	}

	// 3) 身份
	id, err := pool.Identity(r.Scope)
	if err != nil {
		return fail("identity", err)
	}
	r.InstallID = id.InstallationID
	acts := upstream.NewActivities(cli, id)

	// 4) register（幂等，重复调用只更新 last_version）。
	// 注：服务端 register 偶发 503 temporarily_unavailable（同一装机当日多次注册后更容易触发），
	// 此时身份已在服务端存在，summary 照常可用，因此这里只记警告、不中断。
	warn := ""
	if action == "provision" || id.ServerID == "" {
		reg, st, raw, err := acts.Register(token, "", "")
		if err != nil {
			warn = fmt.Sprintf("register http %d: %s", st, short(string(raw)))
			id.RecordError("register", fmt.Errorf("http %d: %s", st, short(string(raw))))
			_ = id.Save()
		} else {
			id.RecordRegister(reg.Installation.ID, reg.Installation.Edition, reg.Installation.Platform,
				reg.Installation.LastVersion, reg.Installation.Status)
			_ = id.Save()
		}
	}
	r.ServerID, r.Status, r.DeviceScope = id.ServerID, id.Status, id.DeviceScope
	r.Nickname = a.Nickname

	// 5) summary（顺手把 credits / onboarding 进度记进身份文件）
	sum, st, raw, err := acts.Summary(token)
	if err != nil {
		return fail("summary", fmt.Errorf("http %d: %s", st, short(string(raw))))
	}
	id.RecordSummary(sum.Credits.Available, sum.Onboarding.EarnedPoints)
	r.DailyStatus = sum.Daily.Status
	r.Streak = sum.Daily.StreakDay
	r.Credits = sum.Credits.Available
	r.Onboarding, r.OnboardingMax = sum.Onboarding.EarnedPoints, sum.Onboarding.MaxPoints
	r.NextEligible = sum.Daily.NextEligibleAt
	r.ClaimDate, r.ClaimedTotal = id.LastClaimDate, id.LastClaimPoints

	switch action {
	case "provision", "status":
		// 只登记+观测，不领；但 summary 观测值要落盘（否则身份文件里的 credits 永远停在旧值）
		_ = id.Save()
	case "claim":
		if sum.Daily.Status != "eligible_today" {
			id.RecordClaim(sum.Daily.ServerDate, "already_granted", sum.Daily.Points, sum.Daily.StreakDay)
			_ = id.Save()
			break
		}
		cl, st, raw, err := acts.Claim(token, sum.Daily.ServerDate)
		if err != nil {
			return fail("claim", fmt.Errorf("http %d: %s", st, short(string(raw))))
		}
		id.RecordClaim(cl.ServerDate, cl.Status, cl.Points, cl.StreakDay)
		r.DailyStatus, r.Streak, r.ClaimedTotal, r.ClaimDate = cl.Status, cl.StreakDay, cl.Points, cl.ServerDate
		_ = id.Save()
	case "reconcile":
		st, raw, err := acts.Reconcile(token)
		if err != nil || st != http.StatusOK {
			return fail("reconcile", fmt.Errorf("http %d: %s", st, short(string(raw))))
		}
		var rec struct {
			Completed []string `json:"completed_task_keys"`
			Evidence  any      `json:"evidence"`
		}
		_ = json.Unmarshal(raw, &rec)
		if len(rec.Completed) > 0 {
			// 结算后立即刷新一次，落盘新的 earned 值
			if s2, _, _, e := acts.Summary(token); e == nil {
				id.RecordSummary(s2.Credits.Available, s2.Onboarding.EarnedPoints)
				r.Credits, r.Onboarding = s2.Credits.Available, s2.Onboarding.EarnedPoints
			}
		}
		_ = id.Save()
	case "referral", "referral-code", "referral-pick":
		// 邀请资格/档位（权威值来自服务端）
		me, st, raw, err := acts.ReferralMe(token)
		if err != nil {
			return fail("referrals/me", fmt.Errorf("http %d: %s", st, short(string(raw))))
		}
		id.RecordReferral(me)
		r.InviteTier = me.Referral.TierGrantedCount
		r.InviteBudget = id.InviteBudgetLeft()
		r.InviteNext = me.NextAutoReward()
		r.InviteTotal = me.Referral.TotalGranted
		eligible := me.Referral.EligibleToInvite
		r.InviteEligible = &eligible
		r.InviteCode = id.ReferralInviteCode
		_ = id.Save()
		if action == "referral-code" || (action == "referral-pick" && r.InviteCode == "") {
			cs, st, raw, err := acts.ReferralCode(token)
			if err != nil {
				return fail("referrals/code", fmt.Errorf("http %d: %s", st, short(string(raw))))
			}
			id.RecordReferralCode(cs)
			r.InviteCode = cs.InviteCode
			r.Note = "share=" + cs.ShareURL
			_ = id.Save()
		} else if action == "referral" {
			r.Note = fmt.Sprintf("next=%d eligible=%v slots=%d", me.NextAutoReward(), eligible, me.Referral.RemainingSlots)
		}
	case "referral-relations":
		rel, st, raw, err := acts.ReferralRelations(token, opt.limit, opt.cursor)
		if err != nil {
			return fail("referrals/relations", fmt.Errorf("http %d: %s", st, short(string(raw))))
		}
		r.Note = fmt.Sprintf("relations=%d", len(rel.Relations))
		if rel.NextCursor != nil && *rel.NextCursor != "" {
			r.Note += " next=" + *rel.NextCursor
		}
	case "referral-bind":
		// 补填邀请码：只有「新手机号验证」账号能成功，老号会 409 existing_user_ineligible。
		if opt.state == "" {
			return fail("referrals/bind", fmt.Errorf("缺少 -state（从 /r/<code> 页面取 referral_state=rs1.…）"))
		}
		st, raw, err := acts.ReferralBind(token, opt.state)
		if err != nil {
			return fail("referrals/bind", fmt.Errorf("http %d: %s", st, short(string(raw))))
		}
		if st < 200 || st >= 300 {
			// 409 existing_user_ineligible = 老账号不能补填（协议事实，不要重试）
			return fail("referrals/bind", fmt.Errorf("http %d: %s", st, short(string(raw))))
		}
		r.Note = "bind ok"
	case "probe":
		body, _ := json.Marshal(map[string]any{
			"model": model, "max_tokens": 64, "stream": false,
			"messages": []map[string]any{{"role": "user", "content": prompt}},
		})
		code, out, err := acts.Messages(token, sum.DesktopInferenceToken, body, upstream.InferenceOpt{
			SessionID: newSessionID(),
		})
		if err != nil {
			return fail("probe", err)
		}
		if code != http.StatusOK {
			return fail("probe", fmt.Errorf("http %d: %s", code, short(string(out))))
		}
		var msg struct {
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(out, &msg)
		text := ""
		for _, c := range msg.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		r.Note = fmt.Sprintf("probe ok model=%s tokens=%d/%d %s", msg.Model,
			msg.Usage.InputTokens, msg.Usage.OutputTokens, trunc(strings.ReplaceAll(text, "\n", " "), 40))
		_ = id.Save()
	}
	if warn != "" && r.Error == "" {
		r.Error = warn
	}
	return r
}

// rotateIdentity 把某账号的装机身份「换一台机器」：旧身份改名备份，生成新 identity+d_i，
// 再走一次 register。适用于安装被风控/409 冲突后的恢复；旧文件保号便于事后对账。
func rotateIdentity(pool *upstream.Pool, key string, accounts []*auth.Auth) (string, error) {
	scope := key
	if !strings.HasPrefix(key, "cm") { // 不是 cuid 就当成 uid，映射到 scope
		for _, a := range accounts {
			if a.UID == key && a.UID != "" {
				if id, err := pool.Identity(a.UID); err == nil {
					scope = id.AccountScope
				}
			}
		}
	}
	backup, err := pool.Rotate(scope)
	if err != nil {
		return "", err
	}
	_ = backup // 备份文件名同目录，rotate 输出里会提示
	return scope, nil
}

func newSessionID() string {
	// RFC4122 v4，与桌面端 UI 生成的会话 id 同形
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(rand.Intn(256))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func sleepJitter(d time.Duration) {
	if d <= 0 {
		return
	}
	time.Sleep(d/2 + time.Duration(rand.Int63n(int64(d))))
}

func printTable(rows []row) {
	fmt.Printf("%-26s %-14s %-9s %-6s %-7s %-9s %-12s %-4s %s\n",
		"账号", "昵称", "daily", "streak", "credits", "onboarding", "inviteCode", "tier", "note/error")
	for _, r := range rows {
		note := r.Note
		if note != "" && r.Error != "" {
			note += " | "
		}
		fmt.Printf("%-26s %-14s %-9s %-6d %-7d %-9s %-12s %-4s %s\n",
			trunc(r.UID, 26), trunc(r.Nickname, 14), trunc(r.DailyStatus, 9), r.Streak, r.Credits,
			fmt.Sprintf("%d/%d", r.Onboarding, r.OnboardingMax),
			trunc(r.InviteCode, 12), fmt.Sprintf("%d/%d", r.InviteTier, r.InviteBudget), note+r.Error)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func short(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func fatal(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(2)
}
