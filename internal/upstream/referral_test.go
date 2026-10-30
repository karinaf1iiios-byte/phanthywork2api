package upstream

import "testing"

func meWithTiers(granted, next, total int, eligible bool) *ReferralMe {
	m := &ReferralMe{}
	m.Referral.TierGrantedCount = granted
	m.Referral.NextRewardPoints = next
	m.Referral.TotalGranted = total
	m.Referral.EligibleToInvite = eligible
	return m
}

// 前两档（5,000 / 8,000）自动到账，第 3 档起人工审核 —— 池子只调度前两档。
func TestReferralAutoTiers(t *testing.T) {
	cases := []struct {
		granted   int
		wantDone  int
		wantNext  int
		wantLeft  int
		exhausted bool
	}{
		{0, 0, 5000, 2, false},
		{1, 1, 8000, 1, false},
		{2, 2, 0, 0, true},
		{3, 2, 0, 0, true}, // 第 3 档起是人工审核，不计入自动预算
	}
	for _, c := range cases {
		m := meWithTiers(c.granted, 0, 0, true)
		if got := m.AutoTiersDone(); got != c.wantDone {
			t.Fatalf("granted=%d AutoTiersDone=%d want %d", c.granted, got, c.wantDone)
		}
		if got := m.NextAutoReward(); got != c.wantNext {
			t.Fatalf("granted=%d NextAutoReward=%d want %d", c.granted, got, c.wantNext)
		}
		if got := m.AutoTiersExhausted(); got != c.exhausted {
			t.Fatalf("granted=%d AutoTiersExhausted=%v", c.granted, got)
		}
		id := &Identity{ReferralTierGranted: m.Referral.TierGrantedCount}
		if got := id.InviteBudgetLeft(); got != c.wantLeft {
			t.Fatalf("granted=%d InviteBudgetLeft=%d want %d", c.granted, got, c.wantLeft)
		}
	}
}

// 没有邀请资格（eligible_to_invite=false）的账号不能当邀请人，哪怕档位没满。
func TestReferralIneligibleCannotInvite(t *testing.T) {
	id := &Identity{}
	id.RecordReferral(meWithTiers(0, 5000, 0, false))
	if id.CanInvite() {
		t.Fatal("eligible=false 的账号不该被选为邀请人")
	}
	id2 := &Identity{}
	id2.RecordReferral(meWithTiers(0, 5000, 0, true))
	if !id2.CanInvite() {
		t.Fatal("eligible=true 且档位未满应可邀请")
	}
}

// 随机选号：只在前两档未满的账号里随机，全池满了两档就返回 nil。
func TestPickInviter(t *testing.T) {
	fresh := &Identity{AccountScope: "a", ReferralTierGranted: 0}
	half := &Identity{AccountScope: "b", ReferralTierGranted: 1}
	full := &Identity{AccountScope: "c", ReferralTierGranted: 2}
	manual := &Identity{AccountScope: "d", ReferralTierGranted: 3}
	bad := &Identity{AccountScope: "e", ReferralTierGranted: 0}
	no := false
	bad.ReferralEligible = &no

	pool := []*Identity{fresh, half, full, manual, bad}
	seen := map[string]int{}
	for i := 0; i < 40; i++ {
		got := PickInviter(pool, func(n int) int { return i % n })
		if got == nil {
			t.Fatal("应能选到邀请人")
		}
		seen[got.AccountScope]++
	}
	if seen["c"] != 0 || seen["d"] != 0 || seen["e"] != 0 {
		t.Fatalf("不该选中已满两档或无资格的账号: %v", seen)
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("两个候选都应被选中过: %v", seen)
	}
	if PickInviter([]*Identity{full, manual}, nil) != nil {
		t.Fatal("全池满档应返回 nil")
	}
}

// RecordReferralCode 只缓存主码 + 分享链接（服务端一次给 5 个 slot）。
func TestRecordReferralCode(t *testing.T) {
	id := &Identity{}
	cs := &ReferralCodes{
		Code:       "EXAMPLE1CODE",
		InviteCode: "ABCD-EFGH-JK23",
		ShareURL:   "https://code.phanthy.com/r/EXAMPLE1CODE",
		Codes:      []ReferralCodeSlot{{Slot: 1, Code: "EXAMPLE1CODE", InviteCode: "ABCD-EFGH-JK23"}},
	}
	id.RecordReferralCode(cs)
	if id.ReferralInviteCode != "ABCD-EFGH-JK23" || id.ReferralShareURL != cs.ShareURL {
		t.Fatalf("邀请码未记录: %+v", id)
	}
}
