// 桌面装机身份（desktop installation）：本地 Ed25519 三把钥匙 + installationId 推导。
//
// 由 PhanthyWork 2.43.0 二进制还原（符号 identity_for_key / load_or_create_signing_key /
// keyring_account_for_scope）：
//
//	identity 钥匙  keyring 账号 = accountScope(= /api/oauth/profile 的 account.uuid)
//	              用途：public_key 字段 + 所有 x-desktop-signature
//	device 钥匙    keyring 账号 = "reward-device-root-v1"（固定名，与账号无关）
//	              用途：device_public_key 字段 + device_signature（绑定串签名）
//	session 钥匙   客户端每次启动由 sidecar 新生成，只作为 inference_public_key 上报；
//	              远端没有 /api/daily-activity/session-key（那是 sidecar 本地端点），
//	              自己造一把等价的新公钥即可，服务端只登记不校验来源。
//
//	installationId = "di_" + base64url(sha256(identity 公钥裸 32B))
//	              服务端会重算这个推导并比对请求头，错了返回 409 desktop_installation_conflict。
package upstream

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"phanthywork2api/internal/fingerprint"
)

// Identity 一台虚拟桌面的身份材料（私钥 seed 三把）。
type Identity struct {
	Path           string `json:"-"`
	AccountScope   string `json:"accountScope"`
	InstallationID string `json:"installationId"`
	IdentitySeed   string `json:"identitySeed"` // base64url(32B)
	DeviceSeed     string `json:"deviceSeed"`   // base64url(32B)
	SessionSeed    string `json:"sessionSeed"`  // base64url(32B)
	// 冗余的可读字段（不参与计算，便于人工核对）
	IdentityPublicKey string `json:"identityPublicKey,omitempty"`
	DevicePublicKey   string `json:"devicePublicKey,omitempty"`
	SessionPublicKey  string `json:"sessionPublicKey,omitempty"`

	// ---- 服务端登记与运营状态（账号池管理用，全部可选、不进签名）----
	ServerID         string `json:"serverInstallationId,omitempty"` // register 返回的 cuid
	Edition          string `json:"edition,omitempty"`
	Platform         string `json:"platform,omitempty"`
	Version          string `json:"version,omitempty"`
	Status           string `json:"status,omitempty"`      // active / ...
	DeviceScope      string `json:"deviceScope,omitempty"` // account | machine
	RegisteredAt     string `json:"registeredAt,omitempty"`
	LastClaimDate    string `json:"lastClaimDate,omitempty"` // 北京时间 YYYY-MM-DD
	LastClaimPoints  int    `json:"lastClaimPoints,omitempty"`
	StreakDay        int    `json:"streakDay,omitempty"`
	OnboardingEarned int    `json:"onboardingEarned,omitempty"`
	CreditsAvailable int    `json:"creditsAvailable,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	LastSeenAt       string `json:"lastSeenAt,omitempty"`

	// 设备画像（platform/arch/osRelease/ipCandidates/deviceId/UA…）。
	// 由种子确定性派生并落盘：同一 di_ 永远报同一台「机器」，换机(rotate)才换画像。
	Profile *fingerprint.Profile `json:"profile,omitempty"`

	// ---- 邀请（开工搭子）运营状态：全部来自 GET /api/oauth/referrals/me ----
	// 权威计数在服务端，本地只是缓存，调度前必须重新拉一次 me 再决定能不能继续邀请。
	ReferralInviteCode  string `json:"referralInviteCode,omitempty"`  // 人读码 ABCD-EFGH-JK23
	ReferralShareURL    string `json:"referralShareUrl,omitempty"`    // https://code.phanthy.com/r/<code>
	ReferralTierGranted int    `json:"referralTierGranted,omitempty"` // 已自动到账档位数（0/1/2，>2 已转人工）
	ReferralTotalPoints int    `json:"referralTotalPoints,omitempty"` // 邀请累计到账积分
	ReferralNextPoints  int    `json:"referralNextPoints,omitempty"`  // 下一档自动奖励（0=已满两档）
	ReferralEligible    *bool  `json:"referralEligible,omitempty"`    // 是否具备邀请资格
	ReferralCheckedAt   string `json:"referralCheckedAt,omitempty"`
}

// DeviceProfile 返回该身份的稳定设备画像（老文件缺字段时按种子补生成并回写）。
func (id *Identity) DeviceProfile() *fingerprint.Profile {
	if id.Profile != nil {
		return id.Profile.Normalize()
	}
	seed, err := base64.RawURLEncoding.DecodeString(id.SessionSeed)
	if err != nil || len(seed) == 0 {
		seed = []byte(id.InstallationID)
	}
	id.Profile = fingerprint.Derive(seed).Normalize()
	return id.Profile
}

// SaveProfile 把画像写回身份文件（Save 会一并落盘）。
func (id *Identity) SaveProfile() {
	_ = id.DeviceProfile()
	if id.Path != "" {
		_ = id.Save()
	}
}

// sessionKey 推理证明签名用的 session 私钥（公钥必须等于 token 的 inference_public_key）。
func (id *Identity) sessionKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(seedOf(id.SessionSeed))
}

func (id *Identity) identityKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(seedOf(id.IdentitySeed))
}
func (id *Identity) deviceKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(seedOf(id.DeviceSeed))
}
func (id *Identity) sessionPub() ed25519.PublicKey {
	return ed25519.NewKeyFromSeed(seedOf(id.SessionSeed)).Public().(ed25519.PublicKey)
}
func (id *Identity) IdentityPublic() string {
	return SPKIPublicKey(id.identityKey().Public().(ed25519.PublicKey))
}
func (id *Identity) DevicePublic() string {
	return SPKIPublicKey(id.deviceKey().Public().(ed25519.PublicKey))
}
func (id *Identity) SessionPublic() string { return SPKIPublicKey(id.sessionPub()) }

// DeriveInstallationID 复刻客户端 identity_for_key：di_ + b64url(sha256(公钥裸 32B))。
func DeriveInstallationID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "di_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// IdentityPath 身份文件路径约定：<dir>/installation-<accountScope>.json
func IdentityPath(dir, accountScope string) string {
	return filepath.Join(dir, "installation-"+accountScope+".json")
}

// LoadOrCreateIdentity 读身份文件；不存在则生成三把钥匙（与 Python 脚本同一套格式）。
func LoadOrCreateIdentity(path, accountScope string) (*Identity, error) {
	if raw, err := os.ReadFile(path); err == nil {
		id := &Identity{}
		if err := json.Unmarshal(raw, id); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", path, err)
		}
		id.Path = path
		if id.AccountScope == "" {
			id.AccountScope = accountScope
		}
		if id.InstallationID == "" {
			id.InstallationID = DeriveInstallationID(id.identityKey().Public().(ed25519.PublicKey))
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	id := &Identity{Path: path, AccountScope: accountScope}
	var err error
	if id.IdentitySeed, err = newSeed(); err != nil {
		return nil, err
	}
	if id.DeviceSeed, err = newSeed(); err != nil {
		return nil, err
	}
	if id.SessionSeed, err = newSeed(); err != nil {
		return nil, err
	}
	id.InstallationID = DeriveInstallationID(id.identityKey().Public().(ed25519.PublicKey))
	if err := id.Save(); err != nil {
		return nil, err
	}
	return id, nil
}

// Save 原子落盘（0600）。
func (id *Identity) Save() error {
	if id.Path == "" {
		return fmt.Errorf("identity: 未设置路径")
	}
	id.IdentityPublicKey = id.IdentityPublic()
	id.DevicePublicKey = id.DevicePublic()
	id.SessionPublicKey = id.SessionPublic()
	raw, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(id.Path), 0o755); err != nil {
		return err
	}
	tmp := id.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, id.Path)
}

func newSeed() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func seedOf(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) != ed25519.SeedSize {
		panic("identity: 私钥 seed 非法")
	}
	return b
}
