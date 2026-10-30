// login 两步登录工具：生成 PKCE 授权链接 → 用户授权取 code → 换 token 落盘 auths/。
//
//	./phanthywork2api-login -step=url
//	./phanthywork2api-login -step=exchange -code=<code 或完整 URL>
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

var (
	baseURL      = flag.String("base-url", "https://code.phanthy.com", "Phanthy base URL")
	outDir       = flag.String("out", "auths", "凭证输出目录")
	step         = flag.String("step", "url", "url|exchange")
	code         = flag.String("code", "", "授权码（exchange 用）")
	verifierFile = ".login-verifier"
)

func main() {
	flag.Parse()
	client := upstream.New(*baseURL)
	switch *step {
	case "url":
		v, ch, err := upstream.NewVerifier()
		if err != nil {
			fatal("verifier: %v", err)
		}
		u := client.AuthorizeURL(ch)
		fmt.Printf("授权链接（浏览器打开，用 Phanthy 账号登录）：\n\n  %s\n\n", u)
		_ = auth.OpenBrowser(u)
		if err := os.WriteFile(verifierFile, []byte(v), 0o600); err != nil {
			fatal("保存 verifier: %v", err)
		}
		fmt.Printf("verifier 已存 %s\n授权后页面跳到 code.phanthy.com/oauth/code/success 显示授权码\n", verifierFile)
	case "exchange":
		if *code == "" {
			fatal("需要 -code 参数")
		}
		raw, err := os.ReadFile(verifierFile)
		if err != nil {
			fatal("读取 verifier 失败（先运行 -step=url）: %v", err)
		}
		code := extractCode(*code)
		tr, err := client.ExchangeCode(code, string(raw))
		if err != nil {
			fatal("token 交换失败: %v\n提示：若 invalid_grant，code 已过期，重新 -step=url", err)
		}

		// ── 身份判定：accountScope 才是唯一键，不能用时间戳 ──────────────
		// 旧实现用 time.Now().Unix() 当文件名，同一个手机号重复登录就多出一个文件，
		// 池子里算两个号（重复烧积分、权重被摊薄）。这里先取 profile，
		// 拿不到就退回 JWT 的 sub（那就是 accountScope），两者都空才用时间戳兜底。
		scope, label := auth.JWTSub(tr.AccessToken), ""
		if prof, perr := client.Profile(tr.AccessToken); perr == nil {
			if prof.Account.UUID != "" {
				scope = prof.Account.UUID
			}
			label = prof.Label()
		} else {
			fmt.Printf("（profile 读取失败，用 token 里的 sub 兜底：%v）\n", perr)
		}
		if label == "" {
			label = "phanthy-" + time.Now().Format("0102")
		}

		expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
		out, deduped, err := auth.SaveLogin(*outDir, scope, tr.AccessToken, tr.RefreshToken,
			tr.TokenType, tr.IDToken, label, expiresAt)
		if err != nil {
			fatal("写文件: %v", err)
		}
		_ = os.Remove(verifierFile)
		fmt.Printf("登录成功！账号 %s（scope=%s）\n凭证: %s（expires_in=%ds）\n", label, scope, out, tr.ExpiresIn)
		if deduped > 0 {
			fmt.Printf("已合并 %d 个同账号的重复凭证（旧文件移到 %s/.trash/）\n", deduped, *outDir)
		}
	default:
		fatal("未知 step: %s", *step)
	}
}

func extractCode(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '#'); i >= 0 { // 页面显示 code#state，剥离 fragment
		s = s[:i]
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" {
		return u.Query().Get("code")
	}
	if i := strings.Index(s, "code="); i >= 0 {
		s = s[i+5:]
		if j := strings.IndexAny(s, "& "); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}

func writeJSON(p string, doc any) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o600)
}

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
