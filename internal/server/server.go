// server.go：Server 装配与路由。
package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"phanthywork2api/internal/auth"
	"phanthywork2api/internal/sanitize"
	"phanthywork2api/internal/session"
	"phanthywork2api/internal/upstream"
)

// Server 网关。
type Server struct {
	cfg    *Config
	client *upstream.Client
	auths  []*auth.Auth
	mu     sync.Mutex

	actsMu     sync.Mutex
	acts       map[string]*upstream.Activities // accountScope -> 活动客户端
	claimState claimState
	pool       *upstream.Pool // 装机身份池（与 cmd/activity 共用同一份文件）

	// ---- 推理面状态 ----
	router   *session.Router       // 会话粘性表（可选落盘）
	scopes   map[string]scopeEntry // uid -> accountScope（30 分钟）
	desk     map[string]deskEntry  // uid -> desktop_inference_token（90s）
	cooldown map[string]time.Time  // uid -> 冷却到期
	cleaner  *sanitize.Cleaner
	rr       uint64

	statPrompt    int64
	statCacheRead int64
	statCacheNew  int64

	// ---- 定时任务（进程内，config.schedule.jobs 驱动）----
	jobs *jobRunner

	// ---- 看板（/ui）----
	startedAt    time.Time
	uiMu         sync.Mutex
	uiOverview   *uiOverview
	uiOverviewAt time.Time
	uiPending    map[string]uiPendingLogin
	uiPhone      map[string]uiPhoneEntry
	regMu        sync.Mutex
	regTry       map[string]time.Time // uid -> 上次 register 失败时刻（10 分钟冷却）
}

// New 装载凭证并构建上游客户端。
func New(cfg *Config) (*Server, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://code.phanthy.com"
	}
	c := upstream.New(cfg.BaseURL)
	if cfg.Upstream.UserAgent != "" {
		c.UserAgent = cfg.Upstream.UserAgent
	}
	s := &Server{
		cfg: cfg, client: c,
		startedAt: time.Now(),
		acts:      map[string]*upstream.Activities{},
		scopes:    map[string]scopeEntry{},
		desk:      map[string]deskEntry{},
		cooldown:  map[string]time.Time{},
	}
	s.pool = upstream.NewPool(cfg.AuthDir, cfg.Identity.DeviceScope)
	// 会话粘性表：可选落盘，网关重启后老会话仍落在原账号上（否则缓存前缀全丢）。
	s.router = session.New(time.Duration(cfg.Session.StickyTTLSeconds)*time.Second, cfg.Session.MaxBindings)
	if cfg.Session.StateFile != "" {
		if err := s.router.Load(cfg.Session.StateFile); err != nil {
			log.Printf("[session] 恢复粘性表失败（忽略）: %v", err)
		} else {
			n, _, _, _ := s.router.Stats()
			log.Printf("[session] 粘性表已恢复 %d 条（%s）", n, cfg.Session.StateFile)
		}
	}
	s.cleaner = sanitize.New(sanitize.Options{
		Disable:              cfg.Sanitize.Disabled,
		KeepBillingHeader:    cfg.Sanitize.KeepBillingHeader,
		ExtraRewrites:        cfg.Sanitize.ExtraRewrites,
		ExtraStripSubstrings: cfg.Sanitize.ExtraStrip,
	})
	// 装载前先按 accountScope 去重：历史上重复登录留下的同一手机号多份凭证
	// 会让池子把一个号算成多个（权重摊薄、重复烧积分）。旧文件移 auths/.trash/。
	if n, err := auth.DedupeDir(cfg.AuthDir); err != nil {
		log.Printf("[auth] 去重扫描失败: %v", err)
	} else if n > 0 {
		log.Printf("[auth] 已合并 %d 个同号重复凭证（旧文件在 %s/.trash/）", n, cfg.AuthDir)
	}
	// 装载凭证（目录不存在=无账号，允许）
	if ents, err := os.ReadDir(cfg.AuthDir); err == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			// installation-*.json 是装机身份，不是账号凭证；空 token 的文件同理跳过。
			if strings.HasPrefix(e.Name(), "installation-") || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			a, err := auth.LoadAuthFile(filepath.Join(cfg.AuthDir, e.Name()))
			if err != nil {
				log.Printf("凭证解析失败 %s: %v", e.Name(), err)
				continue
			}
			if a.AccessToken == "" && a.RefreshToken == "" && a.APIKey == "" {
				continue
			}
			s.auths = append(s.auths, a)
		}
	}
	return s, nil
}

// workdir 仓库根目录（scripts/ 与 data/ 所在处）。
// 以 auths 目录为锚点反推：AuthDir 默认 "./auths"，其父目录就是根。
func (s *Server) workdir() string {
	if wd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(wd, "scripts", "pool.sh")); err == nil {
			return wd
		}
	}
	if abs, err := filepath.Abs(s.cfg.AuthDir); err == nil {
		return filepath.Dir(abs)
	}
	return "."
}

// claimAll 全池签到一次（服务端幂等：领过的号返回 already_granted）。
func (s *Server) claimAll() (int, string, error) {
	var b strings.Builder
	rc := 0
	day := time.Now().Format("2006-01-02")
	for _, a := range s.accounts() {
		if a.NeedsRefresh(5 * time.Minute) {
			s.refreshOne(a)
		}
		sum, cl, err := s.claimOne(a, false)
		if err != nil {
			fmt.Fprintf(&b, "✗ %s %v\n", a.UID, err)
			rc = 1
			continue
		}
		if cl != nil {
			fmt.Fprintf(&b, "✓ %s +%d 分 streak=%d 余额=%d\n", a.UID, cl.Points, cl.StreakDay, sum.Credits.Available)
		} else {
			fmt.Fprintf(&b, "· %s %s 余额=%d\n", a.UID, sum.Daily.Status, sum.Credits.Available)
		}
		s.claimState.mu.Lock()
		if s.claimState.last == nil {
			s.claimState.last = map[string]string{}
		}
		s.claimState.last[a.UID] = day
		s.claimState.mu.Unlock()
	}
	return rc, b.String(), nil
}

// accounts 加锁读一次账号表（claimLoop 起的 goroutine 与登录新增账号会并发）。
func (s *Server) accounts() []*auth.Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*auth.Auth(nil), s.auths...)
}

// Accounts 当前账号列表（状态展示）。
func (s *Server) Accounts() []*auth.Auth { return s.auths }

// Run 启动 HTTP 服务。
func (s *Server) Run() error {
	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{
		Addr:         s.cfg.Listen,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE 长连接
	}
	if len(s.cfg.Schedule.KeepaliveHours) > 0 && s.cfg.Schedule.KeepaliveEnabled {
		go s.keepaliveLoop()
	}
	if len(s.cfg.Schedule.Jobs) > 0 {
		s.jobs = newJobRunner(s, s.cfg.Schedule.Jobs)
		go s.jobs.loop()
		var on []string
		for _, j := range s.cfg.Schedule.Jobs {
			if j.Enabled {
				on = append(on, j.Name+"@"+strings.Join(jobSchedule(j), ","))
			}
		}
		log.Printf("[job] 定时任务已启用 %d 条：%s", len(on), strings.Join(on, "  "))
	}
	// 兼容老配置：没有 schedule.jobs 时才用 claim_hours 的每小时循环
	if s.cfg.Schedule.ClaimEnabled && len(s.cfg.Schedule.Jobs) == 0 {
		go s.claimLoop()
		log.Printf("[claim] 定时领取已启用 hours=%v", s.cfg.Schedule.ClaimHours)
	}
	return srv.ListenAndServe()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handlerHealth)
	// 内置看板与可视化登录（见 ui.go；默认仅本机可访问）
	s.mountUI(mux)
	mux.HandleFunc("/status", s.withAuth(s.handlerStatus))
	mux.HandleFunc("/v1/models", s.withAuth(s.handlerModels))
	mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handlerChat))
	mux.HandleFunc("/v1/messages", s.withAuth(s.handlerMessages))
	// Phanthy 只读代理（积分/活动等）
	mux.HandleFunc("/v1/phanthy/credits", s.withAuth(s.handlerPhanthyCredits))
	mux.HandleFunc("/v1/phanthy/usage", s.withAuth(s.handlerPhanthyUsage))
	mux.HandleFunc("/v1/phanthy/rewards", s.withAuth(s.handlerPhanthyRewards))
	mux.HandleFunc("/v1/phanthy/work-rewards", s.withAuth(s.handlerPhanthyWorkRewards))
	mux.HandleFunc("/v1/phanthy/referrals", s.withAuth(s.handlerPhanthyReferrals))
	mux.HandleFunc("/v1/phanthy/profile", s.withAuth(s.handlerPhanthyProfile))
	mux.HandleFunc("/v1/phanthy/activity", s.withAuth(s.handlerPhanthyActivity))
	mux.HandleFunc("/v1/phanthy/claim", s.withAuth(s.handlerPhanthyClaim))
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" {
			next(w, r)
			return
		}
		h := r.Header.Get("Authorization")
		if !strings.EqualFold(h, "Bearer "+s.cfg.APIKey) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"bad key"}}`))
			return
		}
		next(w, r)
	}
}
