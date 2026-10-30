// Package fingerprint 生成「像一台真实装机」的客户端画像。
//
// 依据 PhanthyWork 2.43.0 macOS sidecar 里 claude-code 的请求构造：
//
//	User-Agent: claude-cli/<il> (${USER_TYPE}, ${CLAUDE_CODE_ENTRYPOINT ?? "cli"})
//	x-app: cli
//	x-phanthy-client-source: desktop|cli      (PHANTHY_CODE_CLIENT_SOURCE)
//	x-phanthy-client-version: <桌面端版本>     (PHANTHY_CODE_CLIENT_VERSION)
//	x-client-request-id / x-request-id: 每次请求一个 uuid（进签名串）
//	X-Claude-Code-Session-Id: 会话 uuid
//	x-phanthy-cli-device: base64url(JSON{version,platform,arch,osRelease,cliVersion,ipCandidates})
//	metadata.user_id: JSON.stringify({device_id,account_uuid,session_id})
//
// 其中 platform/arch/osRelease 来自 node:os（process.platform / os.arch / os.release），
// ipCandidates 是 os.networkInterfaces() 里的 IPv4（真实桌面端会把自己局域网 IP 报上去），
// device_id 是 claude-code 持久化的 32 位 hex（首次启动随机生成，之后固定）。
//
// 这些值全部由装机身份的种子确定性派生：同一个 di_ 每次请求都报同一台「机器」，
// 换机（rotate）就换整套画像，互相之间不会串。
package fingerprint

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// 桌面端与内置 CLI 的版本是两回事：客户端版本 2.43.0，UA 里的 claude-cli 版本 2.1.92。
const (
	DefaultClientVersion = "2.43.0"
	DefaultCLIVersion    = "2.1.92"
)

// Profile 一台模拟设备的稳定画像。除会话 id 外，生命周期内所有字段都不变。
type Profile struct {
	ClientVersion string   `json:"clientVersion"` // x-phanthy-client-version
	CLIVersion    string   `json:"cliVersion"`    // x-phanthy-cli-device.cliVersion + UA
	Platform      string   `json:"platform"`      // darwin | win32
	Arch          string   `json:"arch"`          // x64 | arm64
	OSRelease     string   `json:"osRelease"`     // Darwin 24.6.0 / 10.0.22631
	IPCandidates  []string `json:"ipCandidates"`  // 局域网 IPv4
	DeviceID      string   `json:"deviceId"`      // 32 hex，进 metadata.user_id
	MachineID     string   `json:"machineId"`     // IOPlatformUUID 形态，排障用
	userType      string
	entrypoint    string
}

// Derive 由身份种子确定性生成画像。空种子退化为随机（仅测试用）。
func Derive(seed []byte) *Profile {
	h := func(tag string) []byte {
		s := sha256.New()
		s.Write(seed)
		s.Write([]byte{0})
		s.Write([]byte(tag))
		return s.Sum(nil)
	}
	// 桌面端 2.43.0 的主要分发形态是 macOS x64（官网 dmg）与 Windows x64。
	// 用同一颗种子的不同字节位选择，保证各字段之间不相关。
	d0, d1, d2, d3 := h("platform"), h("arch"), h("os"), h("ip")
	p := &Profile{
		ClientVersion: DefaultClientVersion,
		CLIVersion:    DefaultCLIVersion,
		Platform:      "darwin",
		Arch:          "x64",
		OSRelease:     "24.6.0",
		DeviceID:      hex.EncodeToString(h("device")[:16]),
		MachineID:     machineUUID(h("machine")),
		userType:      "external",
		entrypoint:    "cli",
	}
	switch {
	case d0[0]%10 < 8: // 8 成 darwin，与官网下载页主推一致
		p.Platform = "darwin"
		switch d1[0] % 3 {
		case 0:
			p.Arch = "arm64"
		default:
			p.Arch = "x64"
		}
		mac := []string{"24.6.0", "24.5.0", "24.4.0", "23.6.0", "25.0.0"}
		p.OSRelease = mac[int(d2[0])%len(mac)]
		if p.Arch == "arm64" && strings.HasPrefix(p.OSRelease, "23.") {
			p.OSRelease = "24.6.0"
		}
	default:
		p.Platform = "win32"
		p.Arch = "x64"
		win := []string{"10.0.22631", "10.0.22621", "10.0.19045", "10.0.26100"}
		p.OSRelease = win[int(d2[0])%len(win)]
	}
	// ipCandidates：真实桌面端报的是本机网卡 IPv4。池里每台「机器」给一组
	// 常见家用网段地址，避免所有账号共用同一个 IP 被按设备维度串起来。
	lan := fmt.Sprintf("192.168.%d.%d", 1+int(d3[0])%3, 2+int(d3[1])%250)
	p.IPCandidates = []string{lan}
	if d3[2]%3 == 0 {
		p.IPCandidates = append(p.IPCandidates, fmt.Sprintf("10.%d.%d.%d", int(d3[3])%4, int(d3[4])%250, 2+int(d3[5])%250))
	}
	return p
}

// machineUUID 把 16 字节哈希拼成小写 UUID（第 4 组首位固定 8，符合 IOPlatformUUID 观感）。
func machineUUID(b []byte) string {
	var u [16]byte
	copy(u[:], b)
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	s := hex.EncodeToString(u[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// UserAgent 复刻 claude-cli 的 UA 拼装：claude-cli/<ver> (external, cli)。
func (p *Profile) UserAgent() string {
	ut := p.userType
	if ut == "" {
		ut = "external"
	}
	ep := p.entrypoint
	if ep == "" {
		ep = "cli"
	}
	return fmt.Sprintf("claude-cli/%s (%s, %s)", p.CLIVersion, ut, ep)
}

// CliDeviceHeader 生成 x-phanthy-cli-device（base64url，无 padding）。
// 服务端用它填 usage.client_os（darwin-x64 之类），缺了就是 null。
func (p *Profile) CliDeviceHeader() string {
	doc := map[string]any{
		"version":      1,
		"platform":     p.Platform,
		"arch":         p.Arch,
		"osRelease":    p.OSRelease,
		"cliVersion":   p.CLIVersion,
		"ipCandidates": p.IPCandidates,
	}
	b, _ := json.Marshal(doc)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ClientOS 服务端按此写入 usage（实测形如 darwin-x64）。
func (p *Profile) ClientOS() string { return p.Platform + "-" + p.Arch }

// MetadataUserID 生成 metadata.user_id：claude-code 发的是 JSON 字符串。
// 结构：{"device_id":"<32hex>","account_uuid":"<uuid|空>","session_id":"<uuid>"}
func (p *Profile) MetadataUserID(accountUUID, sessionID string) string {
	b, _ := json.Marshal(map[string]string{
		"device_id":    p.DeviceID,
		"account_uuid": accountUUID,
		"session_id":   sessionID,
	})
	return string(b)
}

// Normalize 补齐缺省字段，供从磁盘读回的老身份文件使用。
func (p *Profile) Normalize() *Profile {
	if p == nil {
		return Derive([]byte("fallback"))
	}
	if p.ClientVersion == "" {
		p.ClientVersion = DefaultClientVersion
	}
	if p.CLIVersion == "" {
		p.CLIVersion = DefaultCLIVersion
	}
	if p.Platform == "" {
		p.Platform = "darwin"
	}
	if p.Arch == "" {
		p.Arch = "x64"
	}
	if p.OSRelease == "" {
		p.OSRelease = "24.6.0"
	}
	if p.DeviceID == "" {
		p.DeviceID = hex.EncodeToString(sha256.New().Sum([]byte(p.MachineID))[:16])
	}
	if p.MachineID == "" {
		p.MachineID = machineUUID([]byte(p.DeviceID + p.Platform))
	}
	if p.userType == "" {
		p.userType = "external"
	}
	if p.entrypoint == "" {
		p.entrypoint = "cli"
	}
	return p
}
