// 邀请（referral / 开工搭子）协议。
//
// 端点来源：PhanthyWork 2.43.0 macOS claude-sidecar 的本地活动代理路由表
// （var ZK9 = {..., "referral-summary","referral-code","referral-relations"}），
// 线上上游路径与签名规则同每日签到，都是 activity canonical v1（换行分隔，
// canonical 里的 path 含 query string）。
//
//	GET  /api/oauth/referrals/me                       邀请资格 + 档位 + 统计
//	POST /api/oauth/referrals/code                     生成/取回专属邀请码（必须带 idempotency-key！）
//	GET  /api/oauth/referrals/relations?limit=N        邀请关系列表（游标分页）
//	POST /api/oauth/referrals/bind  {"state":"rs1..."} 绑定邀请关系（仅「新手机号验证」账号可用）
//
// 三个实测坑：
//  1. POST /referrals/code 不带 idempotency-key → 503 dependency_unavailable；
//     带上幂等键（参与 canonical 第 7 行）才是 200。这不是「服务维护」，是签名/幂等约束。
//  2. 邀请码有两层：code（URL 用的长 token，/r/<code>）+ invite_code（人读的 ABCD-EFGH-JK23），
//     服务端一次返回 5 个 slot，每个 slot 一对，不能只存第一个。
//  3. bind 的 state 来自 GET /r/<code> 渲染出来的 referral_state=rs1.<id>.<token>；
//     已存在的账号 bind 会被拒 409 existing_user_ineligible（"Only a new verified phone account can bind."），
//     即「补填邀请码」只能发生在注册当时，老账号无法事后补绑。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ReferralTierRewards 三档奖励（前两档自动到账，第 3 档起人工审核）。
var ReferralTierRewards = []int{5000, 8000, 10000}

// ReferralMe 是 GET /referrals/me 的响应。
type ReferralMe struct {
	SchemaVersion int `json:"schema_version"`
	Referral      struct {
		ProgramStatus      string  `json:"program_status"`
		EligibleToInvite   bool    `json:"eligible_to_invite"`
		EligibilityReason  *string `json:"eligibility_reason"`
		RewardPoints       int     `json:"reward_points"`
		RewardTiers        []int   `json:"reward_tiers"`
		TierGrantedCount   int     `json:"tier_granted_count"`
		TierAllocatedCount int     `json:"tier_allocated_count"`
		NextRewardPoints   int     `json:"next_reward_points"`
		DailyCommitted     int     `json:"daily_committed"`
		DailyGranted       int     `json:"daily_granted"`
		DailyLimit         int     `json:"daily_limit"`
		TotalGranted       int     `json:"total_granted"`
		PendingCount       int     `json:"pending_count"`
		TotalOccupied      int     `json:"total_occupied"`
		TotalLimit         int     `json:"total_limit"`
		RemainingSlots     int     `json:"remaining_slots"`
		RecentRelations    []struct {
			Status string `json:"status"`
			Tier   int    `json:"tier"`
			Points int    `json:"points"`
		} `json:"recent_relations"`
		InvitedProgress json.RawMessage `json:"invited_progress"`
	} `json:"referral"`
}

// AutoTiersDone 返回「已自动到账的档位数」（只有前两档是自动的，第 3 档起要人工审核）。
// 这个值就是账号池「一个邀请人只做两次」的判断依据。
func (m *ReferralMe) AutoTiersDone() int {
	n := m.Referral.TierGrantedCount
	if n > 2 {
		return 2
	}
	if n < 0 {
		return 0
	}
	return n
}

// AutoTiersExhausted 前两档（5,000 / 8,000）是否都已到账。
func (m *ReferralMe) AutoTiersExhausted() bool { return m.AutoTiersDone() >= 2 }

// NextAutoReward 下一档还能自动拿多少分；已满两档返回 0。
func (m *ReferralMe) NextAutoReward() int {
	done := m.AutoTiersDone()
	if done >= len(ReferralTierRewards) || done >= 2 {
		return 0
	}
	pts := m.Referral.NextRewardPoints
	if pts == 0 {
		pts = ReferralTierRewards[done]
	}
	return pts
}

// ReferralCodeSlot 一个邀请码槽位（服务端一次给 5 个）。
type ReferralCodeSlot struct {
	Slot       int    `json:"slot"`
	Code       string `json:"code"`
	InviteCode string `json:"invite_code"`
	ShareURL   string `json:"share_url"`
}

// ReferralCodes 是 POST /referrals/code 的响应。
type ReferralCodes struct {
	SchemaVersion int                `json:"schema_version"`
	Code          string             `json:"code"`
	InviteCode    string             `json:"invite_code"`
	ShareURL      string             `json:"share_url"`
	Codes         []ReferralCodeSlot `json:"codes"`
}

// ReferralRelations 是 GET /referrals/relations 的响应。
type ReferralRelations struct {
	SchemaVersion int               `json:"schema_version"`
	Relations     []json.RawMessage `json:"relations"`
	NextCursor    *string           `json:"next_cursor"`
}

// ReferralMe 拉取邀请资格与统计。
func (a *Activities) ReferralMe(token string) (*ReferralMe, int, []byte, error) {
	st, raw, err := a.do(token, http.MethodGet, "/api/oauth/referrals/me", "", nil)
	if err != nil {
		return nil, st, raw, err
	}
	var out ReferralMe
	if err := a.decode(st, raw, &out, "referrals/me"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// ReferralCode 生成/取回专属邀请码。
// 幂等键是硬要求：省掉它服务端回 503 dependency_unavailable（实测）。
func (a *Activities) ReferralCode(token string) (*ReferralCodes, int, []byte, error) {
	path := "/api/oauth/referrals/code"
	idem := fmt.Sprintf("referral-code:%s:%d", a.id.InstallationID, time.Now().UnixMilli())
	st, raw, err := a.do(token, http.MethodPost, path, idem, []byte("{}"))
	if err != nil {
		return nil, st, raw, err
	}
	var out ReferralCodes
	if err := a.decode(st, raw, &out, "referrals/code"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// ReferralRelations 邀请关系列表；limit 默认 20，游标从上一页 next_cursor 传回。
func (a *Activities) ReferralRelations(token string, limit int, cursor string) (*ReferralRelations, int, []byte, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	st, raw, err := a.do(token, http.MethodGet, "/api/oauth/referrals/relations?"+q.Encode(), "", nil)
	if err != nil {
		return nil, st, raw, err
	}
	var out ReferralRelations
	if err := a.decode(st, raw, &out, "referrals/relations"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// ReferralBind 用 /r/<code> 页面里的 referral_state 绑定邀请关系。
// 只对新手机号验证账号有效；老账号会得到 409 existing_user_ineligible，
// 这个错误是协议事实，不要重试。
func (a *Activities) ReferralBind(token, state string) (int, []byte, error) {
	body, _ := json.Marshal(map[string]string{"state": state})
	path := "/api/oauth/referrals/bind"
	idem := fmt.Sprintf("referral-bind:%s:%d", a.id.InstallationID, time.Now().UnixMilli())
	return a.do(token, http.MethodPost, path, idem, body)
}
