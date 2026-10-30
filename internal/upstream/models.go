// Code generated from PhanthyWork 2.43.0 sidecar (claude-sidecar)  -- 不要手改。

package upstream

import "strings"

// Catalog 客户端内置的模型 → 上下文窗口表（sidecar 里的 vx5 常量）。
// 它不等于「你的套餐允许的模型」：允许列表在服务端，按 plan 走；这里只用于
// /v1/models 暴露候选 id 与上下文长度，以及客户端做预算提示。
var Catalog = map[string]int{
	"apollo-1.0":              262144,
	"apollo-1.1":              262144,
	"apollo-2.0":              1048576,
	"claude-haiku-4-5":        200000,
	"claude-opus-4-7":         1000000,
	"claude-opus-4-8":         1000000,
	"claude-sonnet-4-6":       200000,
	"deepseek-chat":           1048576,
	"deepseek-reasoner":       1048576,
	"deepseek-v4-flash":       1048576,
	"deepseek-v4-pro":         1048576,
	"gaia-1.0":                200000,
	"gaia-1.1":                1000000,
	"gaia-1.2":                1000000,
	"glm-4.5":                 128000,
	"glm-4.5-air":             128000,
	"glm-4.6":                 200000,
	"glm-4.7":                 200000,
	"glm-5":                   200000,
	"glm-5-turbo":             200000,
	"glm-5.1":                 200000,
	"glm-5.2":                 1000000,
	"glm-5.3-flash":           1000000,
	"iris-1.0":                1048576,
	"kimi-k2-0905-preview":    262144,
	"kimi-k2-thinking":        262144,
	"kimi-k2-thinking-turbo":  262144,
	"kimi-k2-turbo-preview":   262144,
	"kimi-k2.5":               262144,
	"kimi-k2.6":               262144,
	"kimi-k2.7-code":          262144,
	"kimi-k3":                 1048576,
	"metis-1.0":               200000,
	"metis-1.1":               1000000,
	"metis-1.2-flash":         1000000,
	"minimax-m2":              204800,
	"minimax-m2.1":            204800,
	"minimax-m2.1-highspeed":  204800,
	"minimax-m2.5":            204800,
	"minimax-m2.5-highspeed":  204800,
	"minimax-m2.7":            204800,
	"minimax-m2.7-highspeed":  204800,
	"phanthy-fast":            1050000,
	"phanthy-pro":             1050000,
	"phanthy-ultra":           1050000,
	"phanthy.com/deepseek-v4": 1048576,
	"z-ai/glm-5.2":            1000000,
	"zeus-1.0-pro":            400000,
	"zeus-1.1":                1050000,
	"zeus-1.1-fast":           1050000,
	"zeus-1.1-pro":            1050000,
}

// ContextLength 查上下文窗口。名字归一化规则与 sidecar 一致：去尾部 [1m]/[2m]、
// 转小写、多段名取最后一段（如 phanthy.com/deepseek-v4 → deepseek-v4）。
func ContextLength(name string) int {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, suf := range []string{"[1m]", "[2m]"} {
		n = strings.TrimSuffix(n, suf)
	}
	n = strings.TrimSpace(n)
	if n == "" {
		return 0
	}
	candidates := []string{n}
	if i := strings.LastIndexAny(n, "/:"); i >= 0 && i+1 < len(n) {
		candidates = append(candidates, n[i+1:])
	}
	for _, c := range candidates {
		if v, ok := Catalog[c]; ok {
			return v
		}
	}
	return 0
}
