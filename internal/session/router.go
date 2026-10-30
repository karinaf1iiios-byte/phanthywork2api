// Package session 做「会话粘性路由」：同一段会话必须落到同一个账号。
//
// 为什么需要：Phanthy 的桌面推理通道按会话记账——
//
//   - 证明签名串里带 x-claude-code-session-id，服务端把会话 id、装机身份、
//     访问令牌绑在一起校验（signed_session_match）；
//   - 追问（follow_up_success）按「同一会话多轮」结算，换账号等于换一台机器，
//     会话断链，onboarding 与用量统计都会归零；
//   - 上下文缓存（cache_control）按会话前缀命中，换账号 = 前缀重建 = 全价重算。
//
// 所以路由策略是「先按会话找绑定，找不到才分配」，而不是每请求轮询。
// 绑定带 TTL：会话闲置超过 TTL 后释放，避免长时间运行后把整池都占满。
package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultTTL 会话绑定存活时间。Claude Code 的一次任务通常几分钟到半小时。
const DefaultTTL = 2 * time.Hour

type binding struct {
	UID     string `json:"uid"`
	Model   string `json:"model,omitempty"`
	Expires int64  `json:"expires"` // unix 秒
}

// Router 会话 → 账号 的粘性表。
type Router struct {
	mu      sync.Mutex
	m       map[string]*binding
	ttl     time.Duration
	max     int
	hits    int64
	misses  int64
	drops   int64
	now     func() time.Time
	Persist string // 非空则每次变更落盘（网关重启后会话不断链）
}

// New 构造。max<=0 时默认 20000 条。
func New(ttl time.Duration, max int) *Router {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if max <= 0 {
		max = 20000
	}
	return &Router{m: map[string]*binding{}, ttl: ttl, max: max, now: time.Now}
}

// Pick 返回该会话应使用的账号。
//   - key 为空表示无会话上下文（一次性调用），直接交给 choose 选；
//   - 已有绑定且 bindOK(uid) 通过 → 复用（sticky=true）；
//   - 否则用 choose 选一个，并写入绑定。
func (r *Router) Pick(key, model string, choose func() string, bindOK func(uid string) bool) (uid string, sticky bool) {
	if key == "" {
		return choose(), false
	}
	r.mu.Lock()
	b, ok := r.m[key]
	now := r.now()
	if ok && b.Expires > now.Unix() && (bindOK == nil || bindOK(b.UID)) {
		b.Expires = now.Add(r.ttl).Unix()
		r.hits++
		uid = b.UID
		r.mu.Unlock()
		return uid, true
	}
	r.mu.Unlock()

	uid = choose()
	if uid == "" {
		return "", false
	}
	r.Bind(key, uid)
	return uid, false
}

// Bind 写入/刷新绑定。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	r.mu.Lock()
	r.missed()
	r.m[key] = &binding{UID: uid, Expires: r.now().Add(r.ttl).Unix()}
	r.evictLocked()
	r.mu.Unlock()
	r.persist()
}

func (r *Router) missed() { r.misses++ }

// Drop 解绑（上游 401/429/模型不允许时调用，下个请求换账号）。
func (r *Router) Drop(key string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	delete(r.m, key)
	r.drops++
	r.mu.Unlock()
	r.persist()
}

// DropUID 账号失效时清掉它名下所有会话，避免继续粘到一个坏账号。
func (r *Router) DropUID(uid string) int {
	r.mu.Lock()
	n := 0
	for k, b := range r.m {
		if b.UID == uid {
			delete(r.m, k)
			n++
		}
	}
	r.mu.Unlock()
	if n > 0 {
		r.persist()
	}
	return n
}

// Stats 返回 绑定量/命中/未命中/解绑。
func (r *Router) Stats() (bindings int, hits, misses, drops int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m), r.hits, r.misses, r.drops
}

// Prune 清理过期绑定，返回清理条数。
func (r *Router) Prune() int {
	r.mu.Lock()
	n := 0
	now := r.now().Unix()
	for k, b := range r.m {
		if b.Expires <= now {
			delete(r.m, k)
			n++
		}
	}
	r.mu.Unlock()
	if n > 0 {
		r.persist()
	}
	return n
}

// evictLocked 超量时按「最早过期」淘汰。调用方已持锁。
func (r *Router) evictLocked() {
	if len(r.m) <= r.max {
		return
	}
	// 先清过期，再按过期时间淘汰，直到回到上限的 9 成。
	now := r.now().Unix()
	for k, b := range r.m {
		if b.Expires <= now {
			delete(r.m, k)
		}
	}
	for len(r.m) > r.max {
		var victim string
		best := int64(1<<62 - 1)
		for k, b := range r.m {
			if b.Expires < best {
				best = b.Expires
				victim = k
			}
		}
		if victim == "" {
			break
		}
		delete(r.m, victim)
	}
}

// persist 原子写盘（临时文件 + rename），失败只影响重启恢复，不影响在用路由。
func (r *Router) persist() {
	if r.Persist == "" {
		return
	}
	r.mu.Lock()
	cp := make(map[string]*binding, len(r.m))
	now := r.now().Unix()
	for k, b := range r.m {
		if b.Expires > now {
			bb := *b
			cp[k] = &bb
		}
	}
	r.mu.Unlock()
	b, err := json.Marshal(map[string]any{"version": 1, "ttl": int(r.ttl.Seconds()), "bindings": cp})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.Persist), 0o755); err != nil {
		return
	}
	tmp := r.Persist + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.Persist)
}

// Load 从磁盘恢复（不存在视为空表）。
func (r *Router) Load(path string) error {
	r.Persist = path
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc struct {
		Bindings map[string]*binding `json:"bindings"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	r.mu.Lock()
	now := r.now().Unix()
	for k, v := range doc.Bindings {
		if v != nil && v.Expires > now {
			r.m[k] = v
		}
	}
	r.mu.Unlock()
	return nil
}
