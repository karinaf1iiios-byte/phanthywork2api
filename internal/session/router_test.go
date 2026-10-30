package session

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStickyReusesBinding(t *testing.T) {
	r := New(time.Hour, 10)
	calls := 0
	choose := func() string {
		calls++
		if calls == 1 {
			return "acct-a"
		}
		return "acct-b"
	}
	uid, sticky := r.Pick("sid:1", "phanthy-pro", choose, nil)
	if uid != "acct-a" || sticky {
		t.Fatalf("首次应新建绑定: uid=%s sticky=%v", uid, sticky)
	}
	uid, sticky = r.Pick("sid:1", "phanthy-pro", choose, nil)
	if uid != "acct-a" || !sticky {
		t.Fatalf("同会话应粘住 acct-a: uid=%s sticky=%v", uid, sticky)
	}
	if calls != 1 {
		t.Fatalf("粘住时不应该再调 choose，实际 %d 次", calls)
	}
	// 另一段会话应分配新账号
	uid, _ = r.Pick("sid:2", "phanthy-pro", choose, nil)
	if uid != "acct-b" {
		t.Fatalf("新会话应走 choose，得到 %s", uid)
	}
	_, hits, _, _ := r.Stats()
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}

func TestBindOKFalseRebinds(t *testing.T) {
	r := New(time.Hour, 10)
	r.Bind("sid:1", "bad")
	uid, sticky := r.Pick("sid:1", "", func() string { return "good" }, func(u string) bool { return u != "bad" })
	if uid != "good" || sticky {
		t.Fatalf("绑定失效应重选: %s %v", uid, sticky)
	}
}

func TestTTLExpiry(t *testing.T) {
	r := New(50*time.Millisecond, 10)
	now := time.Now()
	r.now = func() time.Time { return now }
	r.Bind("sid:1", "a")
	now = now.Add(time.Second)
	uid, sticky := r.Pick("sid:1", "", func() string { return "b" }, nil)
	if uid != "b" || sticky {
		t.Fatalf("过期后应重选: %s %v", uid, sticky)
	}
}

func TestDropUID(t *testing.T) {
	r := New(time.Hour, 10)
	r.Bind("s1", "a")
	r.Bind("s2", "a")
	r.Bind("s3", "b")
	if n := r.DropUID("a"); n != 2 {
		t.Fatalf("DropUID=%d，期望 2", n)
	}
	if n, _, _, _ := r.Stats(); n != 1 {
		t.Fatalf("剩余绑定 %d", n)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	r := New(time.Hour, 10)
	r.Persist = path
	r.Bind("sid:x", "acct")
	r2 := New(time.Hour, 10)
	if err := r2.Load(path); err != nil {
		t.Fatal(err)
	}
	uid, sticky := r2.Pick("sid:x", "", func() string { return "other" }, nil)
	if uid != "acct" || !sticky {
		t.Fatalf("恢复失败: %s %v", uid, sticky)
	}
}

func TestEvictCap(t *testing.T) {
	r := New(time.Hour, 3)
	for i := 0; i < 10; i++ {
		r.Bind(string(rune('a'+i)), "acct")
	}
	if n, _, _, _ := r.Stats(); n > 3 {
		t.Fatalf("绑定数 %d 超过上限 3", n)
	}
}
