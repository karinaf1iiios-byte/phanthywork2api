package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"phanthywork2api/internal/upstream"
)

func modelsServer() *Server {
	s := &Server{cfg: DefaultConfig()}
	s.cfg.Models = map[string]string{
		"glm-5.3":         "glm-5.3",     // 直通
		"claude-opus-4-8": "phanthy-pro", // 别名
	}
	return s
}

// /v1/models 必须同时给出配置里的名字和内置目录，且带上上下文窗口。
func TestHandlerModelsMergesCatalog(t *testing.T) {
	s := modelsServer()
	rec := httptest.NewRecorder()
	s.handlerModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Data []struct {
			ID            string `json:"id"`
			Upstream      string `json:"upstream"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	type entry struct {
		Up  string
		Ctx int
	}
	byID := map[string]entry{}
	seen := map[string]int{}
	for _, d := range out.Data {
		seen[d.ID]++
		byID[d.ID] = entry{d.Upstream, d.ContextLength}
	}
	if _, ok := byID["glm-5.3"]; !ok {
		t.Fatal("配置里的直通模型 glm-5.3 没出现")
	}
	if e, ok := byID["claude-opus-4-8"]; !ok || e.Up != "phanthy-pro" {
		t.Fatalf("别名未暴露 upstream 字段: %+v", e)
	}
	if _, ok := byID["kimi-k3"]; !ok {
		t.Fatal("内置目录里的 kimi-k3 没出现")
	}
	if e := byID["kimi-k3"]; e.Ctx != 1048576 {
		t.Fatalf("kimi-k3 上下文窗口 = %d，期望 1048576", e.Ctx)
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("模型 %s 重复出现 %d 次", id, n)
		}
	}
}

// 未知模型必须原样透传，不能被静默改成 default_model。
func TestMapModelPassthrough(t *testing.T) {
	s := modelsServer()
	s.cfg.Upstream.DefaultModel = "phanthy-pro"

	if got, ok := s.mapModel("glm-5.3"); !ok || got != "glm-5.3" {
		t.Fatalf("直通失败: %q %v", got, ok)
	}
	if got, ok := s.mapModel("claude-opus-4-8"); !ok || got != "phanthy-pro" {
		t.Fatalf("别名失败: %q %v", got, ok)
	}
	// 上游目录比客户端新：Phana-5.5o 不在内置表里，也必须透传
	if got, ok := s.mapModel("Phana-5.5o"); !ok || got != "Phana-5.5o" {
		t.Fatalf("新模型未透传: %q %v", got, ok)
	}
	// 空 model 才回落 default
	if got, _ := s.mapModel(""); got != "phanthy-pro" {
		t.Fatalf("空模型应回落 default，得到 %q", got)
	}
	// strict 模式下未知模型拒绝
	s.cfg.Upstream.StrictModels = true
	if _, ok := s.mapModel("Phana-5.5o"); ok {
		t.Fatal("strict_models=true 时未知模型应被拒绝")
	}
	if got, ok := s.mapModel("glm-5.3"); !ok || got != "glm-5.3" {
		t.Fatalf("strict 模式下已知模型仍应通过: %q %v", got, ok)
	}
}

func TestContextLengthNormalize(t *testing.T) {
	cases := map[string]int{
		"glm-5.3-flash":           1000000,
		"glm-4.6":                 200000,
		"glm-5.3":                 0, // 网页已有、内置表还没有：不猜
		"GLM-5.2":                 1000000,
		"kimi-k3[1m]":             1048576,
		"phanthy.com/deepseek-v4": 1048576,
		"unknown-model":           0,
		"":                        0,
	}
	for in, want := range cases {
		if got := upstream.ContextLength(in); got != want {
			t.Errorf("ContextLength(%q) = %d, want %d", in, got, want)
		}
	}
}

// config.example.json 里的 13 个模型必须全部出现在 /v1/models —— 早版本这里是空的原因。
func TestExampleConfigModelsAllListed(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Skipf("读不到 config.example.json: %v", err)
	}
	var cfg struct {
		Models map[string]string `json:"models"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}
	s := &Server{cfg: DefaultConfig()}
	s.cfg.Models = cfg.Models
	rec := httptest.NewRecorder()
	s.handlerModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	have := map[string]bool{}
	for _, d := range out.Data {
		have[strings.ToLower(d.ID)] = true
	}
	want := []string{"Phanthy-fast", "Phanthy-pro", "Phanthy-ultra", "glm-5.3-flash", "glm-5.3",
		"glm-5.2", "kimi-k3", "kimi-k2.7-code", "deepseek-v4.1-flash-1017",
		"Phana-5.5o", "Phana-5.5s", "Phano-6a", "Phano-6.1s"}
	for _, w := range want {
		if !have[strings.ToLower(w)] {
			t.Errorf("/v1/models 缺 %s", w)
		}
	}
}
