package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/upstream"
)

// tokenServer 假的 /oauth/token：每次刷新换一个新 access token，并统计被调次数。
func tokenServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(hits, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("access-%d", n),
			"refresh_token": fmt.Sprintf("refresh-%d", n),
			"expires_in":    1800,
			"token_type":    "Bearer",
		})
	}))
}

func testAuthWithFile(t *testing.T, left time.Duration) *auth.Auth {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "phanthy-test.json")
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  "old-access",
			"refreshToken": "old-refresh",
			"expiresAt":    time.Now().Add(left).Unix(),
			"tokenType":    "Bearer",
		},
		"account": map[string]any{"uid": "phanthy-test", "nickname": "t"},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := auth.LoadAuthFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// 剩余寿命低于阈值 → 续期并落盘；这是旧实现做不到的（旧版首轮要等 1 小时）。
func TestRefreshIfNeededRenewsNearExpiry(t *testing.T) {
	var hits int32
	srv := tokenServer(t, &hits)
	defer srv.Close()

	a := testAuthWithFile(t, 2*time.Minute)
	s := &Server{client: upstream.New(srv.URL)}

	if !s.refreshIfNeeded(a, 10*time.Minute) {
		t.Fatal("剩余 2 分钟、阈值 10 分钟，应当续期")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("刷新次数 = %d，期望 1", got)
	}
	a.Lock()
	tok, left := a.AccessToken, time.Duration(a.ExpiresAt-time.Now().Unix())*time.Second
	a.Unlock()
	if tok != "access-1" {
		t.Fatalf("access token 未更新: %q", tok)
	}
	if left < 29*time.Minute {
		t.Fatalf("expiresAt 没按 expires_in 更新，剩余 %s", left)
	}
	// 落盘：重新读文件应当是新的 refresh token
	re, err := auth.LoadAuthFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	re.Lock()
	rt := re.RefreshToken
	re.Unlock()
	if rt != "refresh-1" {
		t.Fatalf("落盘的 refresh token = %q，期望 refresh-1", rt)
	}
}

// 剩余寿命还长 → 一次都不打上游。
func TestRefreshIfNeededSkipsFreshToken(t *testing.T) {
	var hits int32
	srv := tokenServer(t, &hits)
	defer srv.Close()

	a := testAuthWithFile(t, 25*time.Minute)
	s := &Server{client: upstream.New(srv.URL)}
	if s.refreshIfNeeded(a, 10*time.Minute) {
		t.Fatal("还剩 25 分钟，不该续期")
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("不该打上游，实际打了 %d 次", got)
	}
}

// 并发续期只能有一次真正打上游（refresh_token 轮换，多刷会把号刷废）。
func TestRefreshSingleflight(t *testing.T) {
	var hits int32
	srv := tokenServer(t, &hits)
	defer srv.Close()

	a := testAuthWithFile(t, time.Minute)
	s := &Server{client: upstream.New(srv.URL)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	refreshed := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.refreshIfNeeded(a, 10*time.Minute) {
				mu.Lock()
				refreshed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("8 个 goroutine 同时续期，上游应只被调用 1 次，实际 %d 次", got)
	}
	if refreshed != 1 {
		t.Fatalf("应只有 1 个 goroutine 报告刷新成功，实际 %d", refreshed)
	}
}

// keepaliveOnce 扫全池：只续该续的号。
func TestKeepaliveOnceSweepsPool(t *testing.T) {
	var hits int32
	srv := tokenServer(t, &hits)
	defer srv.Close()

	due := testAuthWithFile(t, 30*time.Second)
	fresh := testAuthWithFile(t, 30*time.Minute)
	s := &Server{client: upstream.New(srv.URL), auths: []*auth.Auth{due, fresh}}

	s.keepaliveOnce(10 * time.Minute)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("只该续 1 个号，实际调上游 %d 次", got)
	}
	due.Lock()
	dueTok := due.AccessToken
	due.Unlock()
	fresh.Lock()
	freshTok := fresh.AccessToken
	fresh.Unlock()
	if dueTok == "old-access" {
		t.Fatal("快过期的号没续上")
	}
	if freshTok != "old-access" {
		t.Fatal("寿命还长的号被误刷了")
	}
}

// 阈值/间隔的默认值必须满足「阈值 > 间隔」，否则会重现旧 bug（两次 tick 之间烂掉）。
func TestKeepaliveDefaultsSane(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Schedule.KeepaliveIntervalSeconds <= 0 || cfg.Schedule.KeepaliveThresholdSeconds <= 0 {
		t.Fatal("keepalive 间隔/阈值必须有默认值")
	}
	if cfg.Schedule.KeepaliveThresholdSeconds <= cfg.Schedule.KeepaliveIntervalSeconds {
		t.Fatalf("阈值(%ds) 必须大于间隔(%ds)，否则会在两次检查之间错过刷新窗口",
			cfg.Schedule.KeepaliveThresholdSeconds, cfg.Schedule.KeepaliveIntervalSeconds)
	}
	// token 寿命 30 分钟，阈值要留足余量：至少 3 个 tick
	if cfg.Schedule.KeepaliveThresholdSeconds < 3*cfg.Schedule.KeepaliveIntervalSeconds {
		t.Fatalf("阈值(%ds) 太小：至少应是间隔的 3 倍",
			cfg.Schedule.KeepaliveThresholdSeconds)
	}
}
