// ident.go：账号真实身份（accountScope）与展示名（手机号）的判定。
//
// 为什么单独一层：登录时拿到的 token 里没有任何"用户可见 id"，
// 早期实现用时间戳当文件名（phanthy-1700000000），导致同一个手机号
// 重复登录会生成两个文件、在池子里算两个号 —— 必须靠 accountScope 去重。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// JWTSub 读 access token 的 sub claim（= accountScope）。不验签，只解码 payload。
func JWTSub(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	seg := parts[1]
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// PhoneFromEmail 把 `+86 138****0000` / `+86138****0000` 归一成 `138****0000`。
// 上游 profile 的 account.email 和 display_name 都是这个格式（手机号注册时）。
func PhoneFromEmail(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "+86")
	s = strings.TrimPrefix(s, "86")
	s = strings.TrimSpace(s)
	// 形如 138 **** 0000 的散空格也收拾一下
	s = strings.ReplaceAll(s, " ", "")
	return s
}

// lookupScope 打开一个 auth 文件，返回它的 accountScope（优先 JWT sub）。
func lookupScope(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	if s, _ := doc["accountScope"].(string); s != "" {
		return s
	}
	a, _ := doc["auth"].(map[string]any)
	if a == nil {
		a = doc
	}
	if t, _ := a["accessToken"].(string); t != "" {
		return JWTSub(t)
	}
	return ""
}

// FindByScope 找出 auths 目录里所有属于同一个 accountScope 的凭证文件。
// 正常应该 0 或 1 个；多于 1 个说明历史上重复登录过，需要去重。
func FindByScope(dir, scope string) []string {
	if scope == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") ||
			strings.HasPrefix(name, "installation-") {
			continue
		}
		p := filepath.Join(dir, name)
		if lookupScope(p) == scope {
			out = append(out, p)
		}
	}
	return out
}

// DedupeDir 扫描整个 auths 目录，把"同一 accountScope 的多个凭证"合并成一个：
// 保留 mtime 最新的那个（余额/刷新进度都在上面），其余移进 .trash。
//
// 用途：历史上按时间戳命名的重复登录文件（同一手机号 → 多个 phanthy-<ts>.json）
// 在启动时自动收干净，否则看板和权重会把一个号算成两个。
func DedupeDir(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	groups := map[string][]os.DirEntry{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") ||
			strings.HasPrefix(name, "installation-") {
			continue
		}
		if sc := lookupScope(filepath.Join(dir, name)); sc != "" {
			groups[sc] = append(groups[sc], e)
		}
	}
	merged := 0
	for _, list := range groups {
		if len(list) < 2 {
			continue
		}
		// 最新的留下
		sort.Slice(list, func(i, j int) bool {
			fi, _ := list[i].Info()
			fj, _ := list[j].Info()
			return fi.ModTime().After(fj.ModTime())
		})
		trash := filepath.Join(dir, ".trash")
		if err := os.MkdirAll(trash, 0o700); err != nil {
			return merged, err
		}
		for _, dup := range list[1:] {
			dst := filepath.Join(trash, dup.Name()+"."+time.Now().Format("20060102-150405"))
			if err := os.Rename(filepath.Join(dir, dup.Name()), dst); err == nil {
				merged++
			}
		}
	}
	return merged, nil
}
