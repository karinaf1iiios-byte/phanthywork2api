// 桌面推理通道（/v1/messages）：desktop-inference-v2 证明头。
//
// 为什么需要这一层：只带 Bearer 访问令牌请求 /v1/messages 会拿到
// 403 request_not_authorized（"请更新或重新打开客户端后重试"）。
// 服务端要求请求由「桌面装机身份」现场签名，协议来自 PhanthyWork 2.43.0
// macOS sidecar 的 sw8()（本地代理 127.0.0.1:0 转发前注入）：
//
//	canonical = "\n".join([
//	  "desktop-inference-v2", METHOD, pathname, ts_ms, nonce_b64url(24B),
//	  b64url(sha256(body)), b64url(sha256(authorization)),
//	  b64url(sha256(x-phanthy-desktop-session)),
//	  x-request-id, x-claude-code-session-id, x-phanthy-expert-id, x-phanthy-skill-names,
//	])                    // 后四项缺省为空串，仍是独立一行
//	signature = ed25519(session 私钥)   // session 公钥必须等于 token 里的 inference_public_key
//
// x-phanthy-desktop-session 就是 /api/oauth/activities/summary 返回的
// desktop_inference_token（JWT，exp = iat+180s，claim 里带 proof_version=2、
// inference_public_key、installation_id）。它只能当「证明」用，不能当 Bearer。
//
// 实测（2026-10-28，账号 体验版）：
//
//	Bearer=access_token + 四头证明 → 200，模型必须在该套餐允许列表内；
//	Bearer=desktop_inference_token → 401 expired_access_token；
//	x-api-key=desktop_inference_token → 401 missing_login。
package upstream

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"phanthywork2api/internal/fingerprint"
)

// InferenceProofTag 推理证明域标签（v2 协议）。
const InferenceProofTag = "desktop-inference-v2"

// InferenceOpt 单次推理调用的会话/专家/Skill 上下文。
// 这些值会同时进入请求头与签名串，缺一个就对不上（服务端按空串签）。
type InferenceOpt struct {
	RequestID   string               // x-request-id / x-client-request-id（进签名串；空则自动生成）
	SessionID   string               // x-claude-code-session-id；界面侧的 x-phanthy-session 会映射成它
	ExpertID    string               // x-phanthy-expert-id（可空）
	SkillNames  string               // x-phanthy-skill-names，逗号分隔（可空）
	Profile     *fingerprint.Profile // 设备画像（UA / cli-device / x-client-request-id 风格）
	Beta        string               // anthropic-beta，空则用桌面端默认两条
	MaxBytes    int64
	HTTPTimeout time.Duration
}

// NewRequestID 生成一个请求 id（桌面端用 uuid v4）。
func NewRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ProofHeaders 计算四头证明。desktopToken 必须由 Summary 新鲜取得（180s 内）。
func ProofHeaders(sessionKey ed25519.PrivateKey, method, pathname string, body []byte,
	authHeader, desktopToken string, opt InferenceOpt) map[string]string {
	ts := itoa(time.Now().UnixMilli())
	nonce := b64urlRand(24)
	h := func(s string) string { return b64urlBytes(sha256Bytes([]byte(s))) }
	canonical := strings.Join([]string{
		InferenceProofTag, strings.ToUpper(method), pathname, ts, nonce,
		b64urlBytes(sha256Bytes(body)), h(authHeader), h(desktopToken),
		opt.RequestID, opt.SessionID, opt.ExpertID, opt.SkillNames,
	}, "\n")
	return map[string]string{
		"x-phanthy-desktop-session": desktopToken,
		"x-phanthy-proof-timestamp": ts,
		"x-phanthy-proof-nonce":     nonce,
		"x-phanthy-proof-signature": b64urlBytes(ed25519.Sign(sessionKey, []byte(canonical))),
	}
}

// CliDevice 生成 x-phanthy-cli-device（base64url JSON）。服务端用它填 usage 的 client_os。
func (a *Activities) CliDevice(platform, arch, osRelease, cliVersion string) string {
	if platform == "" {
		platform = "darwin"
	}
	if arch == "" {
		arch = "x64"
	}
	if cliVersion == "" {
		cliVersion = "2.43.0"
	}
	doc, _ := json.Marshal(map[string]any{
		"version": 1, "platform": platform, "arch": arch,
		"osRelease": osRelease, "cliVersion": cliVersion, "ipCandidates": []string{},
	})
	return base64.RawURLEncoding.EncodeToString(doc)
}

// Messages 发一条真正的推理请求。body 是 Anthropic Messages 格式的原始 JSON 字节
// （证明签名对字节敏感，不能发第二次序列化的结果）。
// 返回 (status, body)；403/401 时 body 里带服务端 error.code，便于区分
// request_not_authorized / model_not_allowed / expired_access_token。
func (a *Activities) Messages(token, desktopToken string, body []byte, opt InferenceOpt) (int, []byte, error) {
	if desktopToken == "" {
		return 0, nil, fmt.Errorf("inference: desktopToken 为空（先调 Summary）")
	}
	req, err := a.newMessagesRequest(token, desktopToken, body, opt)
	if err != nil {
		return 0, nil, err
	}
	limit := opt.MaxBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	client := *a.c.HTTP
	client.Timeout = timeoutOf(opt)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, raw, nil
}

// MessagesH 同 Messages，但额外返回响应头（x-phanthy-cost-points 等计费提示在里面）。
func (a *Activities) MessagesH(token, desktopToken string, body []byte, opt InferenceOpt) (int, []byte, http.Header, error) {
	if desktopToken == "" {
		return 0, nil, nil, fmt.Errorf("inference: desktopToken 为空（先调 Summary）")
	}
	req, err := a.newMessagesRequest(token, desktopToken, body, opt)
	if err != nil {
		return 0, nil, nil, err
	}
	limit := opt.MaxBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	client := *a.c.HTTP
	client.Timeout = timeoutOf(opt)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return resp.StatusCode, raw, resp.Header, nil
}

// MessagesStream 发流式请求并原样返回响应体（SSE 逐块透传，不做缓冲）。
// 调用方负责关闭 Body。
func (a *Activities) MessagesStream(token, desktopToken string, body []byte, opt InferenceOpt) (*http.Response, error) {
	if desktopToken == "" {
		return nil, fmt.Errorf("inference: desktopToken 为空（先调 Summary）")
	}
	req, err := a.newMessagesRequest(token, desktopToken, body, opt)
	if err != nil {
		return nil, err
	}
	client := *a.c.HTTP
	client.Timeout = 0 // 流式不断连
	return client.Do(req)
}

func timeoutOf(opt InferenceOpt) time.Duration {
	if opt.HTTPTimeout > 0 {
		return opt.HTTPTimeout
	}
	return 180 * time.Second
}

// newMessagesRequest 组装与桌面端一致的请求：UA / 设备画像 / 会话头 / 证明四头。
//
// 实测要点（2026-10-28）：
//   - x-request-id 必须与签名串里的 request_id 一致，否则 403 request_not_authorized；
//   - x-client-request-id 是同一值的第二份头（sidecar 在 firstParty 分支另发一个），
//     缺了不影响校验，但两个都发才像真机；
//   - anthropic-beta 保留 claude-code-20250219：桌面端内核就是 claude-code，删了反而不像。
func (a *Activities) newMessagesRequest(token, desktopToken string, body []byte, opt InferenceOpt) (*http.Request, error) {
	path := "/v1/messages"
	auth := "Bearer " + token
	if opt.RequestID == "" {
		opt.RequestID = NewRequestID()
	}
	prof := opt.Profile
	if prof == nil {
		prof = fingerprint.Derive([]byte(a.id.InstallationID)).Normalize()
	}
	req, err := http.NewRequest(http.MethodPost, a.c.Base+path+"?beta=true", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", prof.UserAgent())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	beta := opt.Beta
	if beta == "" {
		beta = "claude-code-20250219,oauth-2025-04-20"
	}
	req.Header.Set("anthropic-beta", beta)
	req.Header.Set("x-app", "cli")
	req.Header.Set("x-phanthy-client-source", "desktop")
	req.Header.Set("x-phanthy-client-version", prof.ClientVersion)
	req.Header.Set("x-phanthy-cli-device", prof.CliDeviceHeader())
	req.Header.Set("x-client-request-id", opt.RequestID)
	req.Header.Set("x-request-id", opt.RequestID)
	req.Header.Set("Authorization", auth)
	if opt.SessionID != "" {
		// 桌面端 UI 的会话 id：sidecar 把它映射成 x-claude-code-session-id 后转发，
		// 但两个头都会到服务端（实测缺 x-phanthy-session 时会话任务判 signed_session_match=false）。
		req.Header.Set("x-phanthy-session", opt.SessionID)
		req.Header.Set("x-claude-code-session-id", opt.SessionID)
	}
	if opt.ExpertID != "" {
		req.Header.Set("x-phanthy-expert-id", opt.ExpertID)
	}
	if opt.SkillNames != "" {
		req.Header.Set("x-phanthy-skill-names", opt.SkillNames)
	}
	for k, v := range ProofHeaders(a.id.sessionKey(), http.MethodPost, path, body, auth, desktopToken, opt) {
		req.Header.Set(k, v)
	}
	return req, nil
}
