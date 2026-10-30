// 每日活动（daily-activity）全链：register -> summary -> claim -> rewards -> reconcile。
//
// 对应客户端本地路径 -> 线上上游路径：
//
//	/api/daily-activity/register  -> /api/oauth/desktop-installations/register   idem register:<installationId>
//	/api/daily-activity/summary   -> /api/oauth/activities/summary               （无 idem）
//	/api/daily-activity/claim     -> /api/oauth/activities/daily-login/claim     idem daily:<installationId>:<daily.server_date>
//	/api/daily-activity/rewards   -> /api/oauth/rewards?type=daily_login         （GET，空 body 哈希）
//	/api/daily-activity/onboarding/reconcile -> /api/oauth/activities/onboarding/reconcile
//
// 关键坑（都已实测）：claim 的幂等键里的 roundKey 实际是 summary.daily.server_date；
// 只有 summary.daily.status == "eligible_today" 时才该 claim。
package upstream

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Activities 绑定一个 Client 与一台虚拟设备身份。
type Activities struct {
	c  *Client
	id *Identity
}

// NewActivities 构造。
func NewActivities(c *Client, id *Identity) *Activities { return &Activities{c: c, id: id} }

// Identity 返回底层身份（便于排障/展示）。
func (a *Activities) Identity() *Identity { return a.id }

type RegisterResp struct {
	SchemaVersion int    `json:"schema_version"`
	RewardStatus  string `json:"reward_status"`
	Installation  struct {
		ID           string `json:"id"`
		Edition      string `json:"edition"`
		Platform     string `json:"platform"`
		FirstVersion string `json:"first_version"`
		LastVersion  string `json:"last_version"`
		Status       string `json:"status"`
	} `json:"installation"`
}

type SummaryResp struct {
	SchemaVersion int    `json:"schema_version"`
	ServerTime    string `json:"server_time"`
	Daily         struct {
		Status         string `json:"status"` // eligible_today / granted_today / ...
		ServerDate     string `json:"server_date"`
		RewardRecordID string `json:"reward_record_id"`
		Points         int    `json:"points"`
		StreakDay      int    `json:"streak_day"`
		NextStreakDay  int    `json:"next_streak_day"`
		NextPoints     int    `json:"next_points"`
		MaxStreakDay   int    `json:"max_streak_day"`
		NextEligibleAt string `json:"next_eligible_at"`
	} `json:"daily"`
	Credits struct {
		Available int `json:"available"`
		Reserved  int `json:"reserved"`
	} `json:"credits"`
	FeatureFlags struct {
		DailyEnabled bool `json:"daily_enabled"`
	} `json:"feature_flags"`
	Onboarding struct {
		Status         string `json:"status"`
		CompletedCount int    `json:"completed_count"`
		TotalCount     int    `json:"total_count"`
		EarnedPoints   int    `json:"earned_points"`
		MaxPoints      int    `json:"max_points"`
		Tasks          []struct {
			TaskKey        string          `json:"task_key"`
			Title          string          `json:"title"`
			RewardPoints   int             `json:"reward_points"`
			ActionKind     string          `json:"action_kind"`
			ActionConfig   json.RawMessage `json:"action_config"`
			Status         string          `json:"status"`
			RewardRecordID string          `json:"reward_record_id"`
		} `json:"tasks"`
	} `json:"onboarding"`
	// DesktopInferenceToken 180s 有效的桌面推理证明用 JWT（见 inference.go），只能当证明不能当 Bearer。
	DesktopInferenceToken string `json:"desktop_inference_token"`
}

type ClaimResp struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"` // granted / already_granted / ...
	ServerDate    string `json:"server_date"`
	Points        int    `json:"points"`
	StreakDay     int    `json:"streak_day"`
	NextPoints    int    `json:"next_points"`
	Reward        struct {
		ID         string `json:"id"`
		RewardCode string `json:"reward_code"`
		Points     int    `json:"points"`
		Status     string `json:"status"`
		GrantedAt  string `json:"granted_at"`
		ExpiresAt  string `json:"expires_at"`
	} `json:"reward"`
}

// do 发一条已签名请求。path 必须是完整上游路径（/api/oauth/...）。
func (a *Activities) do(token, method, path, idem string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if method == http.MethodPost {
		if body == nil {
			body = []byte("{}")
		}
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, a.c.Base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	sig := SignActivity(a.id.identityKey(), method, path, idem, body)
	req.Header.Set("User-Agent", a.c.UserAgent)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-desktop-installation-id", a.id.InstallationID)
	req.Header.Set("x-desktop-timestamp", sig.Timestamp)
	req.Header.Set("x-desktop-nonce", sig.Nonce)
	req.Header.Set("x-desktop-signature", sig.Signature)
	if idem != "" {
		req.Header.Set("idempotency-key", idem)
	}
	resp, err := a.c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

func (a *Activities) decode(status int, raw []byte, out any, what string) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s http %d: %s", what, status, truncate(string(raw), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse %s: %v: %s", what, err, truncate(string(raw), 300))
	}
	return nil
}

// Register 装机注册（幂等：同一 installationId 重复注册返回同一条记录）。
func (a *Activities) Register(token, platform, version string) (*RegisterResp, int, []byte, error) {
	if platform == "" {
		platform = "macos"
	}
	if version == "" {
		version = "2.43.0"
	}
	ts := itoa(time.Now().UnixMilli())
	nonce := b64urlRand(18)
	sessionPub := a.id.SessionPublic()
	body, err := json.Marshal(map[string]string{
		"public_key":           a.id.IdentityPublic(),
		"edition":              "phanthy_work",
		"platform":             platform,
		"version":              version,
		"device_public_key":    a.id.DevicePublic(),
		"device_signature":     SignDeviceBinding(a.id.deviceKey(), a.id.AccountScope, a.id.InstallationID, a.id.IdentityPublic(), sessionPub, ts, nonce),
		"inference_public_key": sessionPub,
	})
	if err != nil {
		return nil, 0, nil, err
	}
	path := "/api/oauth/desktop-installations/register"
	// 绑定串与请求签名必须共用同一个 ts/nonce（客户端同一对变量，服务端会比对）。
	st, raw, err := a.doWithSig(token, http.MethodPost, path, "register:"+a.id.InstallationID, body, ts, nonce)
	if err != nil {
		return nil, st, raw, err
	}
	var out RegisterResp
	if err := a.decode(st, raw, &out, "register"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// doWithSig 用给定 ts/nonce 重签并发送（register 专用：绑定串已用同一对值签过）。
func (a *Activities) doWithSig(token, method, path, idem string, body []byte, ts, nonce string) (int, []byte, error) {
	req, err := http.NewRequest(method, a.c.Base+path, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	canonical := strings.Join([]string{ActivityTag, method, path, ts, nonce, b64urlBytes(sha256Bytes(body)), idem}, "\n")
	req.Header.Set("User-Agent", a.c.UserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-desktop-installation-id", a.id.InstallationID)
	req.Header.Set("x-desktop-timestamp", ts)
	req.Header.Set("x-desktop-nonce", nonce)
	req.Header.Set("x-desktop-signature", b64urlBytes(ed25519.Sign(a.id.identityKey(), []byte(canonical))))
	req.Header.Set("idempotency-key", idem)
	resp, err := a.c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// Summary 活动状态（daily.status / server_date / credits / onboarding 都在这里）。
func (a *Activities) Summary(token string) (*SummaryResp, int, []byte, error) {
	out, st, raw, err := a.summary(token)
	return out, st, raw, err
}
func (a *Activities) summary(token string) (*SummaryResp, int, []byte, error) {
	body, _ := json.Marshal(map[string]string{"inference_public_key": a.id.SessionPublic()})
	st, raw, err := a.do(token, http.MethodPost, "/api/oauth/activities/summary", "", body)
	if err != nil {
		return nil, st, raw, err
	}
	var out SummaryResp
	if err := a.decode(st, raw, &out, "summary"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// Claim 领取当日积分；roundKey = summary.daily.server_date。
func (a *Activities) Claim(token, roundKey string) (*ClaimResp, int, []byte, error) {
	body := []byte("{}")
	path := "/api/oauth/activities/daily-login/claim"
	idem := fmt.Sprintf("daily:%s:%s", a.id.InstallationID, roundKey)
	st, raw, err := a.do(token, http.MethodPost, path, idem, body)
	if err != nil {
		return nil, st, raw, err
	}
	var out ClaimResp
	if err := a.decode(st, raw, &out, "claim"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// Rewards 领奖记录列表（GET 也是签名请求，body 为空）。
func (a *Activities) Rewards(token string) (int, []byte, error) {
	q := url.Values{}
	q.Set("type", "daily_login")
	return a.do(token, http.MethodGet, "/api/oauth/rewards?"+q.Encode(), "", nil)
}

// Reconcile 让服务端按真实用量结算 onboarding（不自造证据）。
func (a *Activities) Reconcile(token string) (int, []byte, error) {
	path := "/api/oauth/activities/onboarding/reconcile"
	idem := fmt.Sprintf("onboarding:reconcile:%s:latest:%d", a.id.InstallationID, time.Now().UnixMilli())
	return a.do(token, http.MethodPost, path, idem, []byte("{}"))
}

// UsageSummaryResp /api/oauth/usage/summary 的最小投影：调度只需要「积分到期时间」。
//
// 实测响应（体验版）：
//
//	{"schema_version":1,"plan":{"name":"体验版","expires_at":"2026-11-25T17:40:14.634Z"},
//	 "usage_by_day_and_model":[...],"recent_usage":[...]}
//
// 积分是整包按套餐到期日失效（没有按批次的到期时间），所以「快过期积分」在本地
// 用 plan.expires_at 换算紧迫度，见 server/weight.go。
type UsageSummaryResp struct {
	Plan struct {
		Name      string `json:"name"`
		ExpiresAt string `json:"expires_at"`
	} `json:"plan"`
	UsageByDayAndModel []UsageRow `json:"usage_by_day_and_model"`
}

// UsageRow 一条用量记录（按 天×模型×客户端 维度聚合）。
type UsageRow struct {
	Date         string `json:"date"`
	Model        string `json:"model"`
	ClientSource string `json:"client_source"`
	ClientOS     string `json:"client_os"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	CostPoints   int64  `json:"cost_points"`
	RequestCount int64  `json:"request_count"`
}

// UsageSummary 读套餐到期时间与用量汇总。
func (a *Activities) UsageSummary(token string) (*UsageSummaryResp, int, []byte, error) {
	st, raw, err := a.do(token, http.MethodGet, "/api/oauth/usage/summary", "", nil)
	if err != nil {
		return nil, st, raw, err
	}
	var out UsageSummaryResp
	if err := a.decode(st, raw, &out, "usage-summary"); err != nil {
		return nil, st, raw, err
	}
	return &out, st, raw, nil
}

// PlanExpiresAt unix 秒；解析失败返回 0（调用方保持原值）。
func (u *UsageSummaryResp) PlanExpiresAt() int64 {
	if u == nil || u.Plan.ExpiresAt == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, u.Plan.ExpiresAt); err == nil {
		return t.Unix()
	}
	return 0
}
