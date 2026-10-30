// 账号池的装机身份管理（Pool）。
//
// 背景：服务端把一个「桌面装机」认成 (accountScope, installationId) 二元组。
// 一个账号可以有多台装机，但 installationId 必须等于 di_+b64url(sha256(identity 公钥))，
// 也就是说「装一台新机器」= 生成一把新的 identity 私钥，而不是随便编一个 UUID。
//
// 账号池里两种设备钥匙策略，服务端都接受，区别在反滥用画像：
//
//	DeviceScope = "account"（默认）  每个账号一套 identity/device/session 三把钥匙，
//	                                等价于「N 台不同的机器各登录一个账号」。账号之间互不
//	                                牵连；代价是同一 IP 下出现 N 台设备。
//	DeviceScope = "machine"         全池共用一把 device 钥匙（真实客户端的做法：
//	                                keyring 名固定 reward-device-root-v1），等价于
//	                                「一台机器上登录了 N 个账号」。device_public_key 相同，
//	                                服务端一旦做设备维度风控，账号会被串在一起。
//
// 身份文件：<dir>/installation-<accountScope>.json（0600，含私钥 seed，切勿入库/外传）。
// machine 模式额外有 <dir>/device-reward-root.json（只存 device seed）。
//
// 并发：Pool 的读写都加锁；Save 走 临时文件+rename，进程崩溃不会留下半截 JSON。
package upstream

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 设备钥匙作用域。
const (
	DeviceScopeAccount = "account"
	DeviceScopeMachine = "machine"
)

// MachineDeviceFile 全池共用的设备钥匙文件名。
const MachineDeviceFile = "device-reward-root.json"

// machineDevice 只保存 machine 模式下的 device seed。
type machineDevice struct {
	DeviceSeed      string `json:"deviceSeed"`
	DevicePublicKey string `json:"devicePublicKey,omitempty"`
	CreatedAt       string `json:"createdAt,omitempty"`
}

// Pool 一个 auth 目录下的全部装机身份。
type Pool struct {
	Dir         string // auths 目录
	DeviceScope string // account（默认）| machine
	mu          sync.Mutex
	cache       map[string]*Identity
}

// NewPool 构造；dir 通常是配置里的 auth_dir。
func NewPool(dir, deviceScope string) *Pool {
	if deviceScope != DeviceScopeMachine {
		deviceScope = DeviceScopeAccount
	}
	return &Pool{Dir: dir, DeviceScope: deviceScope, cache: map[string]*Identity{}}
}

func (p *Pool) machineDevicePath() string { return filepath.Join(p.Dir, MachineDeviceFile) }

// loadMachineDevice 读（或首建）machine 模式的设备钥匙。
func (p *Pool) loadMachineDevice() (*machineDevice, error) {
	path := p.machineDevicePath()
	if raw, err := os.ReadFile(path); err == nil {
		md := &machineDevice{}
		if err := json.Unmarshal(raw, md); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", path, err)
		}
		if md.DeviceSeed == "" {
			return nil, fmt.Errorf("%s 缺少 deviceSeed", path)
		}
		return md, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	seed, err := newSeed()
	if err != nil {
		return nil, err
	}
	md := &machineDevice{DeviceSeed: seed, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	md.DevicePublicKey = SPKIPublicKey(ed25519.NewKeyFromSeed(seedOf(seed)).Public().(ed25519.PublicKey))
	raw, _ := json.MarshalIndent(md, "", "  ")
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, err
	}
	return md, nil
}

// Identity 取（必要时创建）某账号的装机身份。
// machine 模式下新身份会借用全池共用的 device seed，并立刻回写文件保持幂等。
func (p *Pool) Identity(accountScope string) (*Identity, error) {
	if accountScope == "" {
		return nil, fmt.Errorf("pool: accountScope 为空")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.cache[accountScope]; ok {
		return id, nil
	}
	path := IdentityPath(p.Dir, accountScope)
	// 身份文件不存在时才需要设备钥匙；存在则直接读。
	_, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}
	exists := statErr == nil
	id, err := LoadOrCreateIdentity(path, accountScope)
	if err != nil {
		return nil, err
	}
	id.DeviceScope = p.DeviceScope
	if p.DeviceScope == DeviceScopeMachine {
		md, err := p.loadMachineDevice()
		if err != nil {
			return nil, err
		}
		if id.DeviceSeed != md.DeviceSeed {
			if !exists {
				id.DeviceSeed = md.DeviceSeed
			} else {
				// 已有身份保留自己的钥匙（改钥匙会让服务端侧的 device 绑定失效），只提示。
				id.DeviceScope = DeviceScopeAccount
			}
		}
	}
	if err := id.Save(); err != nil {
		return nil, err
	}
	p.cache[accountScope] = id
	return id, nil
}

// Rotate 换一台「机器」：把现有身份文件改名备份，生成新的 identity+device+session 与新的
// installationId，返回备份路径。调用方随后需要重新 register（新 di_ 在服务端是全新装机）。
// 只替换不并存：一个 accountScope 始终只对应一个在用身份。
func (p *Pool) Rotate(accountScope string) (string, error) {
	src := IdentityPath(p.Dir, accountScope)
	if _, err := os.Stat(src); err != nil {
		return "", fmt.Errorf("pool: 找不到 %s: %w", src, err)
	}
	backup := fmt.Sprintf("%s.bak-%d.json", strings.TrimSuffix(src, ".json"), time.Now().Unix())
	if err := os.Rename(src, backup); err != nil {
		return "", err
	}
	p.Forget(accountScope)
	id, err := p.Identity(accountScope)
	if err != nil {
		return backup, err
	}
	return backup, id.Save()
}

// Forget 从缓存中丢弃某账号的身份（换机/外部改动文件后必须调用，否则仍读到旧私钥）。
func (p *Pool) Forget(accountScope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cache, accountScope)
}

// List 列出目录内全部身份（按 accountScope 排序）。用于巡检/对账。
func (p *Pool) List() ([]*Identity, error) {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Identity
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "installation-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		scope := strings.TrimSuffix(strings.TrimPrefix(name, "installation-"), ".json")
		id, err := LoadOrCreateIdentity(filepath.Join(p.Dir, name), scope)
		if err != nil {
			continue // 坏文件不该拖垮整池巡检
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountScope < out[j].AccountScope })
	return out, nil
}

// RecordRegister 记录一次成功注册返回的装机元信息。
func (id *Identity) RecordRegister(serverID, edition, platform, version, status string) {
	id.ServerID = serverID
	if edition != "" {
		id.Edition = edition
	}
	if platform != "" {
		id.Platform = platform
	}
	if version != "" {
		id.Version = version
	}
	if status != "" {
		id.Status = status
	}
	id.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
	id.LastError = ""
	id.LastSeenAt = id.RegisteredAt
}

// RecordClaim 记录一次领取结果（date 用服务端给的 server_date，避免时区歧义）。
func (id *Identity) RecordClaim(serverDate, status string, points, streakDay int) {
	switch status {
	case "granted":
		id.LastClaimDate = serverDate
		id.LastClaimPoints = points
		id.StreakDay = streakDay
		id.LastError = ""
	case "already_granted":
		id.LastClaimDate = serverDate
		id.StreakDay = streakDay
		id.LastError = ""
	default:
		if status != "" {
			id.LastError = "claim: " + status
		}
	}
	id.LastSeenAt = time.Now().UTC().Format(time.RFC3339)
}

// RecordSummary 记录 summary 观测值（credits / onboarding 进度）。
func (id *Identity) RecordSummary(credits, onboardingEarned int) {
	id.CreditsAvailable = credits
	id.OnboardingEarned = onboardingEarned
	id.LastSeenAt = time.Now().UTC().Format(time.RFC3339)
}

// RecordError 记录失败原因（巡检时能看出哪个账号卡在哪一步）。
func (id *Identity) RecordError(step string, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	id.LastError = step + ": " + msg
	id.LastSeenAt = time.Now().UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------- 邀请（referral）

// RecordReferral 记录一次 GET /api/oauth/referrals/me 观测值。
// 档位/累计/资格全部以服务端为准，本地字段只用于巡检与调度展示。
func (id *Identity) RecordReferral(m *ReferralMe) {
	if m == nil {
		return
	}
	id.ReferralTierGranted = m.Referral.TierGrantedCount
	id.ReferralTotalPoints = m.Referral.TotalGranted
	id.ReferralNextPoints = m.NextAutoReward()
	eligible := m.Referral.EligibleToInvite
	id.ReferralEligible = &eligible
	id.ReferralCheckedAt = time.Now().UTC().Format(time.RFC3339)
	if m.Referral.EligibilityReason != nil && *m.Referral.EligibilityReason != "" {
		id.LastError = "referral: " + *m.Referral.EligibilityReason
	}
}

// RecordReferralCode 记录专属邀请码（服务端一次给多个 slot，这里取 slot 1 作为对外主码）。
func (id *Identity) RecordReferralCode(c *ReferralCodes) {
	if c == nil {
		return
	}
	id.ReferralInviteCode = c.InviteCode
	id.ReferralShareURL = c.ShareURL
	id.ReferralCheckedAt = time.Now().UTC().Format(time.RFC3339)
}

// InviteBudgetLeft 前两档（5,000 / 8,000）还剩几次自动奖励，0 表示该邀请人已做满两次。
func (id *Identity) InviteBudgetLeft() int {
	left := 2 - id.ReferralTierGranted
	if left < 0 {
		return 0
	}
	if left > 2 {
		return 2
	}
	return left
}

// CanInvite 是否还能拿自动奖励：有资格 + 前两档没满。
func (id *Identity) CanInvite() bool {
	if id.ReferralEligible != nil && !*id.ReferralEligible {
		return false
	}
	return id.InviteBudgetLeft() > 0
}

// PickInviter 在身份列表里随机挑一个「前两档还没做满」的邀请人。
// randInt 传入随机源（测试里注入常量，生产用 math/rand）。返回 nil 表示全池都满了。
func PickInviter(ids []*Identity, randInt func(n int) int) *Identity {
	var candidates []*Identity
	for _, id := range ids {
		if id.CanInvite() {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if randInt == nil {
		return candidates[0]
	}
	return candidates[randInt(len(candidates))%len(candidates)]
}
