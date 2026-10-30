package server

import (
	"math/rand"
	"testing"
	"time"

	"phanthywork2api/internal/auth"
)

// 造一个只带运行态的账号（不落盘）。
func testAuth(uid string, credits, planExp, lastUsed int64) *auth.Auth {
	a := &auth.Auth{UID: uid}
	a.SetUsage(credits, planExp)
	if lastUsed > 0 {
		a.MarkUsed(lastUsed)
	}
	return a
}

func newWeightServer() *Server {
	return &Server{cfg: DefaultConfig()}
}

// 快过期要压过额度：同样积分、同样闲置，离到期 1 小时的号权重必须高于 60 天后的号。
func TestWeightExpiringBeatsFarExpiry(t *testing.T) {
	s := newWeightServer()
	now := time.Now()
	soon := testAuth("soon", 5000, now.Add(time.Hour).Unix(), 0)
	far := testAuth("far", 5000, now.Add(60*24*time.Hour).Unix(), 0)
	ws := s.weightOf(soon, 5000, now)
	wf := s.weightOf(far, 5000, now)
	if ws <= wf {
		t.Fatalf("快过期权重 %.3f 应大于远期 %.3f", ws, wf)
	}
	if ws-wf < s.cfg.Pool.ExpiringWeight*0.9 {
		t.Fatalf("快过期加成不足：diff=%.3f 期望≈%.3f", ws-wf, s.cfg.Pool.ExpiringWeight)
	}
}

// urgency 两端钳制：到期日已过 → 仍按 1 计（不反向扣分）；窗口外 → 0。
func TestWeightUrgencyClamped(t *testing.T) {
	s := newWeightServer()
	now := time.Now()
	expired := testAuth("expired", 1000, now.Add(-48*time.Hour).Unix(), 0)
	outside := testAuth("outside", 1000, now.Add(30*24*time.Hour).Unix(), 0)
	base := testAuth("base", 1000, 0, 0) // 没有到期信息 → 不加成
	we, wo, wb := s.weightOf(expired, 1000, now), s.weightOf(outside, 1000, now), s.weightOf(base, 1000, now)
	if we-wb < s.cfg.Pool.ExpiringWeight-0.001 {
		t.Fatalf("已过期应拿满加成：w=%.3f base=%.3f", we, wb)
	}
	if wo != wb {
		t.Fatalf("窗口外不应加成分：w=%.3f base=%.3f", wo, wb)
	}
}

// 闲置因子：从没用过的号拿满额补偿，刚用过的不加。
func TestWeightIdleCompensation(t *testing.T) {
	s := newWeightServer()
	now := time.Now()
	fresh := testAuth("never", 1000, 0, 0)
	hot := testAuth("hot", 1000, 0, now.Unix()) // 刚刚用过 → 闲置 0
	if d := s.weightOf(fresh, 1000, now) - s.weightOf(hot, 1000, now); d < s.cfg.Pool.IdleWeightMax-1e-3 {
		t.Fatalf("闲置补偿应为满额 %.1f，得到 %.3f", s.cfg.Pool.IdleWeightMax, d)
	}
	// 封顶：闲置 1000 小时也只给 max。
	old := testAuth("old", 1000, 0, now.Add(-1000*time.Hour).Unix())
	if got := s.weightOf(old, 1000, now) - s.weightOf(hot, 1000, now); got > s.cfg.Pool.IdleWeightMax+1e-3 {
		t.Fatalf("闲置补偿未封顶：diff=%.1f", got)
	}
}

// 额度因子用全集最大积分口径（workbuddy 的 P1-B 教训）。
func TestWeightUsesFullSetMaxCredits(t *testing.T) {
	s := newWeightServer()
	now := time.Now()
	a := testAuth("a", 1000, 0, 0)
	b := testAuth("b", 4000, 0, 0)
	if m := maxCreditsOf([]*auth.Auth{a, b}); m != 4000 {
		t.Fatalf("maxCredits=%d want 4000", m)
	}
	// 分母（全集最大值）越小，低额度号的"占比"越高 → 权重越大。
	if got := s.weightOf(a, 2000, now) - s.weightOf(a, 4000, now); got <= 0 {
		t.Fatalf("分母变小应让低额度号权重变大，diff=%.3f", got)
	}
}

// 抽签分布：快过期号应显著更容易被选中（2000 次抽样，期望 >70%）。
func TestPickWeightedFavorsExpiring(t *testing.T) {
	s := newWeightServer()
	now := time.Now()
	// 两边都刚用过（闲置 0），只看「快过期」这一项的作用。
	soon := testAuth("soon", 3000, now.Add(2*time.Hour).Unix(), now.Unix())
	far := testAuth("far", 3000, now.Add(90*24*time.Hour).Unix(), now.Unix())
	rng := rand.New(rand.NewSource(42))
	pick := func(n int64) int64 { return rng.Int63n(n) }
	hit := 0
	const rounds = 2000
	for i := 0; i < rounds; i++ {
		if s.pickWeighted([]*auth.Auth{soon, far}, pick).UID == "soon" {
			hit++
		}
	}
	if ratio := float64(hit) / rounds; ratio < 0.7 {
		t.Fatalf("快过期号命中率 %.2f 偏低（期望 >0.70）", ratio)
	}
}

// 单号池直接返回；空池返回 nil；注入 rnd 恒选第一个。
func TestPickWeightedEdges(t *testing.T) {
	s := newWeightServer()
	if s.pickWeighted(nil, nil) != nil {
		t.Fatal("空池应返回 nil")
	}
	one := testAuth("only", 10, 0, 0)
	if got := s.pickWeighted([]*auth.Auth{one}, nil); got != one {
		t.Fatal("单号应原样返回")
	}
	zero := func(int64) int64 { return 0 }
	a, b := testAuth("a", 10, 0, 0), testAuth("b", 10, 0, 0)
	if got := s.pickWeighted([]*auth.Auth{a, b}, zero); got != a {
		t.Fatalf("rnd=0 应选第一个，得到 %s", got.UID)
	}
}
