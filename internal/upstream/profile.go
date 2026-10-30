// profile.go：GET /api/oauth/profile —— 账号的真实身份与展示名。
//
// 实测（2026-10-28）：
//
//	{"account":{"uuid":"cm...","email":"+86 138****0000","display_name":"+86 138****0000",
//	            "created_at":"2026-10-26T17:46:43.759Z"},
//	 "organization":{"uuid":"phanthy-code","name":"PhanthyCode",...}}
//
// 手机号只有掩码（上游就这么返回），所以池子里能显示成 138****0000，拿不到完整号。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Profile 是 /api/oauth/profile 的响应（只取用得上的字段）。
type Profile struct {
	Account struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	} `json:"account"`
	Organization struct {
		UUID             string `json:"uuid"`
		Name             string `json:"name"`
		RateLimitTier    string `json:"rate_limit_tier"`
		BillingType      string `json:"billing_type"`
		SubscriptionFrom string `json:"subscription_created_at"`
	} `json:"organization"`
}

// Profile 用 Bearer 直接读账号资料（这个端点不需要装机签名）。
func (c *Client) Profile(token string) (*Profile, error) {
	st, raw, err := c.Do(token, http.MethodGet, "/api/oauth/profile", nil, nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("profile http %d: %s", st, truncate(string(raw), 200))
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("profile 解析失败: %v", err)
	}
	return &p, nil
}

// Phone 返回归一化后的手机号掩码（`+86 138****0000` → `138****0000`）。
func (p *Profile) Phone() string {
	s := strings.TrimSpace(p.Account.Email)
	if s == "" {
		s = p.Account.DisplayName
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+86")
	s = strings.TrimSpace(s)
	return strings.ReplaceAll(s, " ", "")
}

// Label 池子里展示用的名字：手机号优先，其次 display_name，最后 uuid 末 6 位。
func (p *Profile) Label() string {
	if v := p.Phone(); v != "" {
		return v
	}
	if v := strings.TrimSpace(p.Account.DisplayName); v != "" {
		return v
	}
	if len(p.Account.UUID) > 6 {
		return "…" + p.Account.UUID[len(p.Account.UUID)-6:]
	}
	return p.Account.UUID
}
