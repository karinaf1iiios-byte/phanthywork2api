// 设备签名（Ed25519）：canonical string 与四头生成。
//
// 全部事实来自 PhanthyWork 2.43.0 二进制直读 + 2026-10-28 对
// https://code.phanthy.com 的实测（三条硬约束，任一错都换来 401）：
//
//  1. 分隔符是换行 "\n"，不是空格；
//  2. 签名串里的路径是「线上上游路径」（/api/oauth/...），不是本地前端路径；
//  3. 幂等键由端点决定（register:<installId> / daily:<installId>:<serverDate>），
//     不能拿 nonce 顶替。
package upstream

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"time"
)

// 两个签名域标签（与客户端 sign_desktop_activity / sign_desktop_device_binding 一致）。
const (
	ActivityTag   = "v1"
	DeviceBindTag = "desktop-device-v1"
)

// Signed 一次签名的产物（四头 + 留证用的 canonical string）。
type Signed struct {
	Timestamp      string // x-desktop-timestamp：13 位毫秒
	Nonce          string // x-desktop-nonce：b64url(18B) = 24 字符
	BodyHash       string // b64url(sha256(body))；GET 传空 body 即空串的哈希
	IdempotencyKey string // 写操作唯一键
	Signature      string // b64url(Ed25519(canonical))
	Canonical      string
}

// SignActivity 用 identity 私钥签一条活动请求。
func SignActivity(key ed25519.PrivateKey, method, upstreamPath, idem string, body []byte) *Signed {
	ts := itoa(time.Now().UnixMilli())
	nonce := b64urlRand(18)
	bh := b64urlBytes(sha256Bytes(body))
	canonical := strings.Join([]string{ActivityTag, method, upstreamPath, ts, nonce, bh, idem}, "\n")
	return &Signed{
		Timestamp:      ts,
		Nonce:          nonce,
		BodyHash:       bh,
		IdempotencyKey: idem,
		Signature:      b64urlBytes(ed25519.Sign(key, []byte(canonical))),
		Canonical:      canonical,
	}
}

// DeviceBindingCanonical 组装设备绑定串（换行分隔）。
func DeviceBindingCanonical(accountScope, installationID, identityPub, sessionPub, ts, nonce string) string {
	return strings.Join([]string{DeviceBindTag, accountScope, installationID, identityPub, sessionPub, ts, nonce}, "\n")
}

// SignDeviceBinding 用「设备钥匙」（keyring 固定名 reward-device-root-v1）签绑定串。
// 服务端用 body.device_public_key 验这条签名——用 identity 私钥签会被判 401。
func SignDeviceBinding(deviceKey ed25519.PrivateKey, accountScope, installationID, identityPub, sessionPub, ts, nonce string) string {
	msg := DeviceBindingCanonical(accountScope, installationID, identityPub, sessionPub, ts, nonce)
	return b64urlBytes(ed25519.Sign(deviceKey, []byte(msg)))
}

// SPKI 是 Ed25519 公钥的 SPKI DER 前缀；公钥传输格式 = base64url(DER) 无 padding（44 字符）。
var SPKIPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// SPKIPublicKey 把裸 32B 公钥编码成 App 里用的 publicKey 字符串。
func SPKIPublicKey(pub ed25519.PublicKey) string {
	buf := make([]byte, 0, len(SPKIPrefix)+len(pub))
	buf = append(buf, SPKIPrefix...)
	buf = append(buf, pub...)
	return b64urlBytes(buf)
}

func sha256Bytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func b64urlBytes(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func b64urlRand(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand 失败: " + err.Error()) // 不可恢复
	}
	return b64urlBytes(b)
}

// itoa 避免为一次转换引入 strconv 之外的依赖。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
