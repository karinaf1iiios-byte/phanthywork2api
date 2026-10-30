package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

// 回归：新号第一次进池时，本机身份只生成了 installationId，服务端没登记过，
// summary 会 401 desktop_installation_required。面板以前直接把它当"摘要失败"，
// 于是这个号的所有字段都是 0。修复要求：summary 之前先 ensureRegistered。
func TestSummarySelfRegistersBeforeRead(t *testing.T) {
	var registered atomic.Bool
	var registerCalls, summaryCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "desktop-installations/register"):
			registerCalls.Add(1)
			registered.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":1,"reward_status":"granted","installation":
				{"id":"srv_installation_test","edition":"phanthy_work","platform":"macos",
				 "first_version":"2.43.0","last_version":"2.43.0","status":"active"}}`))
		case strings.Contains(r.URL.Path, "/oauth/profile"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"account":{"uuid":"scopeSELFTEST","email":"+86 138****0000",
				"display_name":"+86 138****0000","created_at":"2026-10-28T00:00:00.000Z"}}`))
		case strings.Contains(r.URL.Path, "/activities/summary"):
			summaryCalls.Add(1)
			if !registered.Load() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"desktop_installation_required",
					"message":"An active desktop installation for this account is required."}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"schema_version":1,"daily":{"status":"eligible_today","server_date":"2026-10-28"},
				"credits":{"available":1500,"reserved":0},
				"feature_flags":{"daily_enabled":true},
				"onboarding":{"status":"in_progress","completed_count":0,"total_count":5,
				              "earned_points":0,"max_points":1500,"tasks":[]}}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.AuthDir = dir
	cfg.Schedule.Jobs = nil // 这个用例只关心 summary 路径
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 造一个"只有本机身份、服务端没登记"的账号
	tok := fakeJWT("scopeSELFTEST")
	if _, err := auth.SaveToken(dir, "phanthy-1700000009", tok, "refresh-x", "Bearer", "",
		"phanthy-1700000009", "138****0000", 4102444800); err != nil {
		t.Fatal(err)
	}
	a, err := auth.LoadAuthFile(filepath.Join(dir, "phanthy-1700000009.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.addAccount(a)

	// 首次读：必须先 register，再 summary 成功
	sum, _, _, err := s.summaryForTries(a, 3)
	if err != nil {
		t.Fatalf("summaryForTries 失败（这就是桌面端 401 的那个 bug）: %v", err)
	}
	if sum.Credits.Available != 1500 {
		t.Errorf("credits=%d want 1500（拿到真实值才说明 summary 通过了）", sum.Credits.Available)
	}
	if registerCalls.Load() != 1 {
		t.Errorf("register 调用 %d 次，want 1", registerCalls.Load())
	}

	// 身份文件要回写 serverInstallationId + status
	raw, _ := os.ReadFile(filepath.Join(dir, "installation-scopeSELFTEST.json"))
	var idoc map[string]any
	_ = json.Unmarshal(raw, &idoc)
	if idoc["serverInstallationId"] != "srv_installation_test" || idoc["status"] != "active" {
		t.Errorf("身份未回写登记结果: %v / %v", idoc["serverInstallationId"], idoc["status"])
	}

	// 第二次读：已登记，不该再 register
	before := registerCalls.Load()
	if _, _, _, err := s.summaryForTries(a, 3); err != nil {
		t.Fatalf("第二次 summary 失败: %v", err)
	}
	if registerCalls.Load() != before {
		t.Errorf("已登记还重复 register：%d → %d", before, registerCalls.Load())
	}
}

func TestIdentityRegisteredPredicate(t *testing.T) {
	cases := []struct {
		name string
		id   *upstream.Identity
		want bool
	}{
		{"都没有", &upstream.Identity{}, false},
		{"只有 status（summary 成功旁证）", &upstream.Identity{Status: "active"}, true},
		{"只有 serverId（register 成功）", &upstream.Identity{ServerID: "srv1"}, true},
		{"都有", &upstream.Identity{ServerID: "srv1", Status: "active"}, true},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := identityRegistered(c.id); got != c.want {
			t.Errorf("%s: identityRegistered=%v want %v", c.name, got, c.want)
		}
	}
}

// fakeJWT 造一个只有 sub 有用的假 token（不验签，只要能被 JWTSub 解出 scope）。
func fakeJWT(sub string) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "exp": 4102444800})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// 身份记录过期（本地写了 status=active，但服务端已经不认这套装机）时，
// summary 会 401 desktop_installation_required —— 必须清掉本地记录、重新登记、再读一次。
func TestSummaryRecoversFromStaleRegistration(t *testing.T) {
	var registered atomic.Bool
	var registerCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "desktop-installations/register"):
			registerCalls.Add(1)
			registered.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"installation":{"id":"srv_new","status":"active","last_version":"2.43.0"}}`))
		case strings.Contains(r.URL.Path, "/oauth/profile"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"account":{"uuid":"scopeSTALE"}}`))
		case strings.Contains(r.URL.Path, "/activities/summary"):
			if !registered.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"desktop_installation_required","message":"An active desktop installation for this account is required."}}`))
				return
			}
			_, _ = w.Write([]byte(`{"credits":{"available":42},"daily":{"status":"eligible_today"},
				"onboarding":{"completed_count":0,"total_count":5,"earned_points":0,"max_points":1500}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.AuthDir = dir
	cfg.Schedule.Jobs = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 本地自认为已登记（status=active），服务端却不认
	idDir := dir
	if err := os.WriteFile(filepath.Join(idDir, "installation-scopeSTALE.json"), []byte(`{
		"accountScope":"scopeSTALE","installationId":"di_stale",
		"identitySeed":"`+strings.Repeat("A", 43)+`","deviceSeed":"`+strings.Repeat("B", 43)+`",
		"sessionSeed":"`+strings.Repeat("C", 43)+`","status":"active","serverInstallationId":"srv_old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SaveToken(dir, "phanthy-1700000010", fakeJWT("scopeSTALE"), "r", "Bearer", "",
		"phanthy-1700000010", "138****0000", 4102444800); err != nil {
		t.Fatal(err)
	}
	a, err := auth.LoadAuthFile(filepath.Join(dir, "phanthy-1700000010.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.addAccount(a)

	sum, _, _, err := s.summaryForTries(a, 2)
	if err != nil {
		t.Fatalf("应从 401 里自愈，实际失败: %v", err)
	}
	if sum.Credits.Available != 42 {
		t.Errorf("credits=%d want 42", sum.Credits.Available)
	}
	if registerCalls.Load() == 0 {
		t.Error("401 desktop_installation_required 之后必须重新 register")
	}
}
