// Package server OpenAI 兼容网关 + Phanthy 只读代理 + 调度器挂载点。
package server

import (
	"encoding/json"
)

// Config 服务配置（环境变量可覆盖，P2A_* 前缀）。
type Config struct {
	Listen    string            `json:"listen"`
	APIKey    string            `json:"api_key"`
	AuthDir   string            `json:"auth_dir"`
	StateFile string            `json:"state_file"`
	BaseURL   string            `json:"base_url"`
	Schedule  ScheduleConfig    `json:"schedule"`
	UI        UIConfig          `json:"ui"`
	Upstream  UpstreamConfig    `json:"upstream"`
	Cooldown  CooldownConfig    `json:"cooldown"`
	Identity  IdentityConfig    `json:"identity"`
	Pool      PoolConfig        `json:"pool"`
	Session   SessionConfig     `json:"session"`
	Cache     CacheConfig       `json:"cache"`
	Sanitize  SanitizeConfig    `json:"sanitize"`
	Models    map[string]string `json:"models"` // 客户端名 → 上游模型名
}

// SessionConfig 会话粘性路由。
type SessionConfig struct {
	// StickyTTLSeconds 会话绑定闲置多久释放（默认 2 小时）。
	StickyTTLSeconds int `json:"sticky_ttl_seconds"`
	// MaxBindings 绑定表上限，超量按最早过期淘汰。
	MaxBindings int `json:"max_bindings"`
	// StateFile 非空则落盘，网关重启后会话不断链（默认 ./data/sessions.json）。
	StateFile string `json:"state_file"`
}

// CacheConfig 前缀缓存（Anthropic cache_control）。
type CacheConfig struct {
	// Disabled=true 时不自动补断点，完全交给调用方。
	Disabled bool `json:"disabled"`
	// RelocateSystem=true 时，超过 SystemInlineBytes 的 system 字符串会被搬到
	// 第一条 user 消息的开头。原因见 docs/05：system 过大时上游只写缓存不读缓存
	// （实测 cc 每次都新建、cr 恒为 0），把大段内容挪进 messages 才能命中。
	RelocateSystem    bool `json:"relocate_system"`
	SystemInlineBytes int  `json:"system_inline_bytes"`
}

// SanitizeConfig 出站净化（见 internal/sanitize）。
type SanitizeConfig struct {
	Disabled          bool        `json:"disabled"`
	KeepBillingHeader bool        `json:"keep_billing_header"`
	ExtraRewrites     [][2]string `json:"extra_rewrites"`
	ExtraStrip        []string    `json:"extra_strip"`
}

// IdentityConfig 账号池的装机身份策略。
type IdentityConfig struct {
	// DeviceScope: "account"（默认，每账号一把 device 钥匙，等价 N 台机器各登录一个账号）
	//              或 "machine"（全池共用一把 device 钥匙，等价一台机器登录 N 个账号）。
	DeviceScope string `json:"device_scope"`
	// ClaimJitterSeconds 账号之间的随机间隔上限，避免同一出口 IP 瞬时打出一串请求。
	ClaimJitterSeconds int `json:"claim_jitter_seconds"`
}

// UIConfig 内置看板（/ui、/api/ui/*）。
type UIConfig struct {
	// Enabled 关闭后 /ui 与 /api/ui 全部不挂载。
	Enabled bool `json:"enabled"`
	// AccessToken 非空时，非本机请求必须带 ?token= 或 X-UI-Token。
	// 空 = 只允许回环/内网地址访问（默认，够本机部署用）。
	AccessToken string `json:"access_token"`
}

type ScheduleConfig struct {
	ClaimHours        []int `json:"claim_hours"`
	OnboardingHours   []int `json:"onboarding_hours"`
	KeepaliveHours    []int `json:"keepalive_hours"`
	ClaimEnabled      bool  `json:"claim_enabled"`
	OnboardingEnabled bool  `json:"onboarding_enabled"`
	KeepaliveEnabled  bool  `json:"keepalive_enabled"`
	// KeepaliveIntervalSeconds keepalive 的检查间隔（默认 60s）。
	KeepaliveIntervalSeconds int `json:"keepalive_interval_seconds"`
	// KeepaliveThresholdSeconds 剩余寿命低于它才续期（默认 600s）。
	// 必须显著大于检查间隔：旧实现是 1h 间隔 + 5min 阈值，空闲号最长会烂 55 分钟。
	KeepaliveThresholdSeconds int `json:"keepalive_threshold_seconds"`
	// Jobs 进程内定时任务（claim / verify / sweep …）。非空时以它为准，
	// ClaimEnabled/ClaimHours 仅作老配置兼容。
	Jobs []JobConfig `json:"jobs"`
}

// PoolConfig 选号权重（三因子，见 server/weight.go）。
type PoolConfig struct {
	// IdleWeightPerHour 闲置补偿：每小时没被选中加多少权重（workbuddy 默认 0.5）。
	IdleWeightPerHour float64 `json:"idle_weight_per_hour"`
	// IdleWeightMax 闲置补偿封顶（默认 5.0）。
	IdleWeightMax float64 `json:"idle_weight_max"`
	// ExpiringWeight 快过期积分加成（默认 20.0：到期当天满额加成）。
	// 参考 workbuddy2api 用 8.0，但那边算的是「快过期占比」，而 Phanthy 是整包积分
	// 随套餐到期一起作废（余量大 = 风险大），所以这里把默认值调高，让紧迫号明显优先。
	ExpiringWeight float64 `json:"expiring_weight"`
	// ExpiryWindowDays 判定"快过期"的窗口（默认 3 天：距到期 3 天内线性升到满额）。
	ExpiryWindowDays float64 `json:"expiry_window_days"`
}

type UpstreamConfig struct {
	TimeoutSeconds int    `json:"timeout_seconds"`
	UserAgent      string `json:"user_agent"`
	InstallationID string `json:"installation_id"`
	// DefaultModel 请求里 model 为空、或 strict_models=false 且名字未知时的兜底。
	DefaultModel string `json:"default_model"`
	// StrictModels=true 时，未在 models 里出现的模型名直接 400，而不是透传给上游。
	// 默认 false：上游的模型目录比客户端内置表更新，透传才不会把新模型吃掉。
	StrictModels bool `json:"strict_models"`
	// ReasoningOnly 决定「上游只回了 thinking、没有正文也没有工具调用」的那一轮怎么交付：
	//   "" / "promote"（默认）：把 thinking 提升为正文。上游这套模型（deepseek 系解析器）会把
	//     答案写进 reasoning 里，原样交付就是「空正文 + finish_reason=stop」，调用方的
	//     reasoning-only 守卫会判成 incomplete turn 并反复重试（一次提问烧 3~4 次额度）。
	//   "passthrough"：原样交付空正文（旧行为），需要调用方自己兜底。
	ReasoningOnly string `json:"reasoning_only"`
}

type CooldownConfig struct {
	HardCredit   string `json:"hard_credit"`
	SoftRate     string `json:"soft_rate"`
	ErrThreshold int    `json:"err_threshold"`
	ErrCooldown  string `json:"err_cooldown"`
}

// DefaultConfig 默认配置。
func DefaultConfig() *Config {
	return &Config{
		Listen:    ":7863",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		BaseURL:   "https://code.phanthy.com",
		Schedule: ScheduleConfig{
			KeepaliveHours:            []int{22},
			KeepaliveEnabled:          true,
			KeepaliveIntervalSeconds:  60,
			KeepaliveThresholdSeconds: 600,
			// 默认全开：签到 / 回读 / 补做（先邀请后 onboarding）。
			// 签到两趟 09:00、21:00 是为了"不漏"，接口幂等；补做排在其后 30/40 分钟，互不重叠。
			Jobs: []JobConfig{
				{Name: "invite", Cmd: "invite", Hours: []int{8, 20}, Minute: 50, Enabled: true},
				{Name: "claim", Cmd: "claim", Hours: []int{9, 21}, Minute: 0, Enabled: true},
				{Name: "verify", Cmd: "verify", Hours: []int{9, 21}, Minute: 30, Enabled: true},
				{Name: "sweep", Cmd: "sweep --apply", Hours: []int{9, 21}, Minute: 40, Enabled: true},
			},
		},
		Upstream: UpstreamConfig{TimeoutSeconds: 120, UserAgent: "claude-cli/2.1.92 (external, cli)",
			DefaultModel: "phanthy-pro", ReasoningOnly: "promote"},
		Identity: IdentityConfig{DeviceScope: "account", ClaimJitterSeconds: 20},
		UI:       UIConfig{Enabled: true},
		Pool:     PoolConfig{IdleWeightPerHour: 0.5, IdleWeightMax: 5, ExpiringWeight: 20, ExpiryWindowDays: 3},
		Session:  SessionConfig{StickyTTLSeconds: 7200, MaxBindings: 20000, StateFile: "./data/sessions.json"},
		Cache:    CacheConfig{SystemInlineBytes: 4096},
		Cooldown: CooldownConfig{HardCredit: "12h", SoftRate: "60s", ErrThreshold: 3, ErrCooldown: "10m"},
		Models:   defaultModels(),
	}
}

// Load 从 JSON 装载（保留默认值，仅覆盖提供的键；env P2A_* 兜底）。
func (c *Config) Load(raw []byte) error {
	base := DefaultConfig()
	// 先落基座，再覆盖
	merged := *base
	out := &merged
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	*c = *out
	c.applyEnv()
	return nil
}

func (c *Config) applyEnv() { /* P2A_* 环境变量覆盖：见 envOrDefault 帮助函数（本骨架最小） */
}

func defaultModels() map[string]string {
	// 体验版只放行 phanthy-pro（Iris-1.0 / Apollo-2.0 实测 403 model_not_allowed），
	// 其它模型名一律映射到它，客户端不必改配置。
	m := map[string]string{}
	for _, name := range []string{
		"phanthy-pro", "phanthy", "claude-opus-4-1", "claude-sonnet-4-5", "claude-sonnet-4",
		"claude-3-7-sonnet-latest", "claude-3-5-sonnet-20241022", "claude-3-5-haiku-latest",
		"gpt-4o", "gpt-4o-mini", "gpt-4.1", "gpt-4.1-mini", "o3-mini", "deepseek-chat",
	} {
		m[name] = "phanthy-pro"
	}
	return m
}
