// Package auth 账号凭证的解析、读写与刷新判定。
// 支持三种落盘形态（对齐官方 CLI / 桌面端 / oauth 工具）：
//
//	CLI 原生:    {"type":"oauth_token","access_token":...,"refresh_token":...,"expires_at":...}
//	嵌套 oauth: {"auth":{"accessToken":...,"refreshToken":...,"expiresAt":...,"tokenType":...},"account":{...}}
//	扁平 oauth: {"accessToken":...,"refreshToken":...,"expiresAt":...,"accountId":...}
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth 单账号凭证（线程安全）。
type Auth struct {
	mu sync.Mutex
	// RefreshMu 刷新单飞锁：refresh_token 每次刷新都会轮换，两个 goroutine 同时刷新
	// 会让其中一个拿到已作废的 refresh_token（服务端 400 invalid_grant）。
	RefreshMu    sync.Mutex
	FilePath     string
	UID          string // account.uid / accountId，也用作 X-Claude-Code-Session-Id
	Nickname     string
	AccessToken  string
	RefreshToken string
	IDToken      string
	APIKey       string // create_api_key 产物（对话 x-api-key 优先）
	TokenType    string
	ExpiresAt    int64 // unix 秒；0=未知
	Raw          map[string]any

	// —— 调度用的运行态（不落 auth 文件，重启后由 activities/usage 回读重建）——
	// Credits 当前可用积分；PlanExpiresAt 套餐/积分到期时间（unix 秒，0=未知）；
	// LastUsed 上次被选出去干活的时间（unix 秒，0=从未）。权重算法见 server/weight.go。
	Credits       int64
	PlanExpiresAt int64
	LastUsed      int64
}

// SetUsage 记录服务端权威的额度与到期时间（activities/summary + usage/summary）。
func (a *Auth) SetUsage(credits, planExpiresAt int64) {
	a.mu.Lock()
	a.Credits = credits
	if planExpiresAt > 0 {
		a.PlanExpiresAt = planExpiresAt
	}
	a.mu.Unlock()
}

// MarkUsed 记一次「这个号被选中执行请求」。
func (a *Auth) MarkUsed(now int64) {
	a.mu.Lock()
	a.LastUsed = now
	a.mu.Unlock()
}

// UsageSnapshot 读一份运行态快照（权重计算用，避免持有锁期间做浮点运算时被写）。
func (a *Auth) UsageSnapshot() (credits, planExpiresAt, lastUsed int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Credits, a.PlanExpiresAt, a.LastUsed
}

func (a *Auth) Lock()   { a.mu.Lock() }
func (a *Auth) Unlock() { a.mu.Unlock() }

// ExpiresIn 剩余秒；未知返回 -1。
func (a *Auth) ExpiresIn() int64 {
	if a.ExpiresAt <= 0 {
		return -1
	}
	return a.ExpiresAt - time.Now().Unix()
}

// NeedsRefresh 距离过期 <= 阈值（默认 5 分钟，官方 Ty6 语义）即需要刷新。
func (a *Auth) NeedsRefresh(threshold time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.RefreshToken == "" {
		return false
	}
	if a.ExpiresAt <= 0 {
		return true
	}
	left := time.Until(time.Unix(a.ExpiresAt, 0))
	return left <= threshold
}

// SaveAtomic 写盘：临时文件 + rename，避免半写。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return saveDocAtomic(a.FilePath, a.Doc())
}

// Doc 规范化输出（嵌套 oauth 形态，与 login 落盘一致）。
func (a *Auth) Doc() map[string]any {
	auth := map[string]any{
		"accessToken":  a.AccessToken,
		"refreshToken": a.RefreshToken,
		"tokenType":    orStr(a.TokenType, "Bearer"),
	}
	if a.IDToken != "" {
		auth["idToken"] = a.IDToken
	}
	if a.ExpiresAt > 0 {
		auth["expiresAt"] = a.ExpiresAt
	}
	if a.APIKey != "" {
		auth["apiKey"] = a.APIKey
	}
	return map[string]any{
		"auth": auth,
		"account": map[string]any{
			"uid":      a.UID,
			"nickname": a.Nickname,
		},
	}
}

// LoadAuths 读取目录下所有凭证文件。
func LoadAuths(dir string) ([]*Auth, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		a, err := LoadAuthFile(p)
		if err != nil {
			return out, fmt.Errorf("parse %s: %w", p, err)
		}
		// 非凭证 JSON（如登录 state 文件）跳过：三种字段全空即视为非凭证
		if a.AccessToken == "" && a.RefreshToken == "" && a.APIKey == "" {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// LoadAuthFile 解析单文件（三形态统一为 Auth）。
func LoadAuthFile(p string) (*Auth, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	a := &Auth{FilePath: p, Raw: root}
	switch {
	case root["type"] == "oauth_token": // CLI 原生
		a.AccessToken, _ = root["access_token"].(string)
		a.RefreshToken, _ = root["refresh_token"].(string)
		a.IDToken, _ = root["id_token"].(string)
		a.TokenType = "Bearer"
		if at, ok := root["expires_at"]; ok {
			a.ExpiresAt = toInt64(at)
		}
		if acct, ok := root["account_id"].(string); ok {
			a.UID = acct
		}
	case root["accessToken"] != nil && strings.Contains(string(raw), "accessToken") && root["auth"] == nil: // 扁平
		a.AccessToken = str(root["accessToken"])
		a.RefreshToken = str(root["refreshToken"])
		a.IDToken = str(root["idToken"])
		a.TokenType = orStr(str(root["tokenType"]), "Bearer")
		if v, ok := root["expiresAt"]; ok {
			a.ExpiresAt = toInt64(v)
		}
		a.UID = orStr(str(root["accountId"]), str(root["uid"]))
		a.Nickname = str(root["nickname"])
		a.APIKey = str(root["api_key"])
	default: // 嵌套
		if authObj, ok := root["auth"].(map[string]any); ok {
			a.AccessToken = str(authObj["accessToken"])
			a.RefreshToken = str(authObj["refreshToken"])
			a.IDToken = str(authObj["idToken"])
			a.TokenType = orStr(str(authObj["tokenType"]), "Bearer")
			if v, ok := authObj["expiresAt"]; ok {
				a.ExpiresAt = toInt64(v)
			}
			a.APIKey = str(authObj["apiKey"])
		}
		if acct, ok := root["account"].(map[string]any); ok {
			a.UID = orStr(str(acct["uid"]), str(acct["accountId"]))
			a.Nickname = str(acct["nickname"])
			if a.APIKey == "" {
				a.APIKey = str(acct["api_key"])
			}
		}
		if a.APIKey == "" {
			a.APIKey = str(root["api_key"])
		}
	}
	if a.UID == "" {
		a.UID = strings.TrimSuffix(filepath.Base(p), ".json")
	}
	return a, nil
}

func saveDocAtomic(p string, doc map[string]any) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case json.Number:
		i, _ := t.Int64()
		return i
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// OpenBrowser 系统浏览器打开授权链接。
func OpenBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", u).Run()
	case "darwin":
		return exec.Command("open", u).Run()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

// SaveToken 把一次 OAuth 换来的凭证落到新文件（登录用）。返回写出的路径。
// 只写 auth/account 两段，字段与桌面端落盘格式一致，LoadAuthFile 能直接读。
func SaveToken(dir, fileStem, access, refresh, tokenType, idToken, uid, nickname string, expiresAt int64) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	authPart := map[string]any{
		"accessToken":  access,
		"refreshToken": refresh,
		"expiresAt":    expiresAt,
		"tokenType":    orStr(tokenType, "Bearer"),
	}
	if idToken != "" {
		authPart["idToken"] = idToken
	}
	doc := map[string]any{
		"auth": authPart,
		"account": map[string]any{
			"uid":      uid,
			"nickname": nickname,
		},
	}
	out := filepath.Join(dir, fileStem+".json")
	if err := saveDocAtomic(out, doc); err != nil {
		return "", err
	}
	return out, nil
}

// SaveLogin 登录落盘：以 accountScope 为唯一键，**同一手机号不会出现第二个文件**。
//
//   - 目录里已有属于该 scope 的凭证 → 覆盖那个文件（保留原文件名，粘性/装机身份都对得上）
//   - 历史重复文件（同一 scope 多于一个）→ 只留最新的一个，其余移进 auths/.trash/
//   - 没有 → 新建 phanthy-<scope>.json
//
// 返回规范化后的路径与被清理掉的重复文件数。
func SaveLogin(dir, scope, access, refresh, tokenType, idToken, nickname string, expiresAt int64) (string, int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	existing := FindByScope(dir, scope)
	stem := ""
	if len(existing) == 0 {
		if scope == "" {
			// 秒级时间戳会在同一秒内撞车（重跑、并发），后缀用纳秒 base36
			now := time.Now()
			stem = "phanthy-" + now.Format("20060102-150405") + "-" + strconv.FormatInt(now.UnixNano()%1e6, 36)
		} else {
			stem = "phanthy-" + scope
		}
	} else {
		stem = strings.TrimSuffix(filepath.Base(existing[0]), ".json")
		// 去重：同一 scope 的多余文件挪进 .trash（不直接删，方便回滚）
		if len(existing) > 1 {
			trash := filepath.Join(dir, ".trash")
			if err := os.MkdirAll(trash, 0o700); err == nil {
				for _, dup := range existing[1:] {
					_ = os.Rename(dup, filepath.Join(trash, filepath.Base(dup)+"."+time.Now().Format("20060102-150405")))
				}
			}
		}
	}
	path, err := SaveToken(dir, stem, access, refresh, tokenType, idToken, stem, nickname, expiresAt)
	merged := 0
	if len(existing) > 1 {
		merged = len(existing) - 1
	}
	return path, merged, err
}

// SetNickname 只改展示名（手机号/昵称），不动余额等运行态。
func (a *Auth) SetNickname(nick string) error {
	a.mu.Lock()
	a.Nickname = nick
	a.mu.Unlock()
	return a.SaveAtomic()
}
