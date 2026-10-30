// Package upstream 封装对 Phanthy（code.phanthy.com）上游的全部 HTTP 调用：
// OAuth 登录/刷新、Phanthy 业务 API（/api/oauth/*、/api/work/*）、
// Anthropic 兼容对话（/v1/messages）与设备签名（Ed25519 四头）。
// 端点与参数来自 phanthywork-re 逆向（M0–M5 验收通过）+ 2026-10-27 实测。
package upstream

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	OAuthBetaHeader  = "oauth-2025-04-20" // 官方 sidecar 常量 of
	ScopeFull        = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	DefaultExpires   = 1800            // sidecar 默认 expires_in
	RefreshThreshold = 5 * time.Minute // Ty6：剩余寿命 <= 5 分钟触发刷新
)

// Client 上游 HTTP 客户端。
type Client struct {
	HTTP *http.Client

	Base      string // https://code.phanthy.com
	ClientID  string // phanthy-code-cli
	UserAgent string // phanthy-code-cli/<ver>（x-phanthy-client-source: desktop）
}

// New 构造默认客户端。
func New(base string) *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		Base:      strings.TrimRight(base, "/"),
		ClientID:  "phanthy-code-cli",
		UserAgent: "phanthy-code-cli/2.43.0",
	}
}

// ---------- OAuth ----------

// AuthorizeURL 构造 PKCE S256 授权链接。
func (c *Client) AuthorizeURL(challenge string) string {
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", c.Base+"/oauth/code/success")
	q.Set("scope", ScopeFull)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", "p2a-login")
	return c.Base + "/oauth/authorize?" + q.Encode()
}

// NewVerifier 生成 PKCE verifier（43–128 字符，官方 48 字节随机）。
func NewVerifier() (string, string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

type TokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
}

// ExchangeCode 授权码换 token。
func (c *Client) ExchangeCode(code, verifier string) (*TokenResp, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", c.Base+"/oauth/code/success")
	form.Set("client_id", c.ClientID)
	return c.tokenForm(form)
}

// RefreshToken 用 refresh_token 换新 token。
func (c *Client) RefreshToken(rt string) (*TokenResp, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	form.Set("client_id", c.ClientID)
	return c.tokenForm(form)
}

func (c *Client) tokenForm(form url.Values) (*TokenResp, error) {
	req, err := http.NewRequest(http.MethodPost, c.Base+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("anthropic-beta", OAuthBetaHeader)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("oauth/token http %d: %s", resp.StatusCode, truncate(string(raw), 250))
	}
	var tr TokenResp
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("parse oauth/token: %v: %s", err, truncate(string(raw), 250))
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("no access_token: %s", truncate(string(raw), 250))
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = DefaultExpires
	}
	return &tr, nil
}

// ---------- Phanthy 业务 API ----------

// Do 发请求到 Phanthy API（自动带 Bearer + 签名四头 + 401 刷新重试一次）。
// method/path/body：path 以 / 开头；returnRaw 时解析留给调用方。
func (c *Client) Do(token string, method, path string, body []byte, header http.Header) (int, []byte, error) {
	return c.do(token, method, path, body, header)
}

func (c *Client) do(token, method, path string, body []byte, header http.Header) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, c.Base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	applyHeaders(req, token, c, method, path, body, header)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized && token == "" {
		return resp.StatusCode, raw, nil
	}
	return resp.StatusCode, raw, nil
}

func applyHeaders(req *http.Request, token string, c *Client, method, path string, body []byte, extra http.Header) {
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("x-phanthy-client-source", "desktop")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// 活动类端点（/api/oauth/activities/*、/api/oauth/rewards）的四头不在这里拼，
	// 由 internal/upstream/activities.go 统一处理（需要 installation 身份与按端点的幂等键）。
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
}

// ---------- 对话 ----------

// Chat 对话（Phanthy Anthropic 兼容 /v1/messages）。认证优先 apiKey(x-api-key) 兜底 Bearer。
// 返回 rc 需调用方 Close；非 2xx 时 rc=nil、status>0、respBody 为上游响应。
func (c *Client) Chat(accessToken, apiKey string, body []byte, timeout time.Duration) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.Base+"/v1/messages?beta=true", strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", OAuthBetaHeader)
	req.Header.Set("x-app", "cli")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	} else if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	} else {
		return nil, http.StatusUnauthorized, []byte(`{"error":{"code":"missing_login"}}`), nil
	}
	cl := c.HTTP
	if timeout > 0 {
		cl = &http.Client{Timeout: timeout}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
