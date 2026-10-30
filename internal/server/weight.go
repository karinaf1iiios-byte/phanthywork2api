// 账号选择权重：三因子加权（借鉴 workbuddy2api 的 weightOf + pickWeighted）。
//
// 为什么要权重：体验版每个号都是「签到 + onboarding 攒出来的积分，整包按套餐到期日失效」，
// 只按轮询/字典序选号会出现两种浪费：
//  1. 快到期的那包积分一次没用，到期直接作废；
//  2. 某个号额度堆得最高却一直排在后面，别人先被抽干。
//
// 三因子（权重是"相对概率"，不是阈值）：
//
//	w = 1
//	  + credits / maxCredits × 10          额度因子：钱多的号多干活（maxCredits 用全集口径）
//	  + urgency × expiring_weight          快过期因子：越接近套餐到期，整包积分越该先花
//	  + idle 补偿（每小时 idle_weight_per_hour，封顶 idle_weight_max）
//
// urgency = clamp(1 - 剩余天数 / expiry_window_days, 0, 1)：
// 距到期还有 ≥ window 天 → 0（不急）；到期当天 → 1（最急）。
// Phanthy 没有暴露「按批次过期」的字段（只有 plan.expires_at），所以整包积分一起计紧迫度。
//
// 抽签用定点整数（×1e6，保底 1），避免低权重号被浮点截断成 0 概率、彻底饿死。
package server

import (
	"math/rand"
	"time"

	"phanthywork2api/internal/auth"
)

const weightScale = 1_000_000

// weightOf 单账号权重。now 传进来便于测试。
func (s *Server) weightOf(a *auth.Auth, maxCredits int64, now time.Time) float64 {
	credits, planExp, lastUsed := a.UsageSnapshot()

	w := 1.0

	// 1) 额度因子
	if maxCredits > 0 && credits > 0 {
		w += float64(credits) / float64(maxCredits) * 10
	}

	// 2) 快过期因子
	if credits > 0 && planExp > 0 {
		window := s.cfg.Pool.ExpiryWindowDays
		if window <= 0 {
			window = 3
		}
		daysLeft := float64(planExp-now.Unix()) / 86400
		urgency := 1 - daysLeft/window
		if urgency < 0 {
			urgency = 0
		}
		if urgency > 1 {
			urgency = 1
		}
		expiringWeight := s.cfg.Pool.ExpiringWeight
		if expiringWeight < 0 {
			expiringWeight = 0
		}
		w += urgency * expiringWeight
	}

	// 3) 闲置补偿
	perHour := s.cfg.Pool.IdleWeightPerHour
	maxIdle := s.cfg.Pool.IdleWeightMax
	if perHour < 0 {
		perHour = 0
	}
	if lastUsed <= 0 {
		w += maxIdle // 从没被用过 → 拿满分，避免冷号永远轮不上
	} else {
		idle := now.Sub(time.Unix(lastUsed, 0)).Hours() * perHour
		if idle < 0 {
			idle = 0 // 时钟回拨
		}
		if idle > maxIdle {
			idle = maxIdle
		}
		w += idle
	}
	return w
}

// maxCreditsOf 全集口径的最大积分（workbuddy 的教训：按"可能候选集"算会让权重随淘汰而漂移）。
func maxCreditsOf(accs []*auth.Auth) int64 {
	var max int64
	for _, a := range accs {
		if c, _, _ := a.UsageSnapshot(); c > max {
			max = c
		}
	}
	return max
}

// pickWeighted 按权重抽签。rnd 为 nil 时用全局 rand.Int63n（测试可注入）。
func (s *Server) pickWeighted(accs []*auth.Auth, rnd func(int64) int64) *auth.Auth {
	if len(accs) == 0 {
		return nil
	}
	if len(accs) == 1 {
		return accs[0]
	}
	now := time.Now()
	maxCredits := maxCreditsOf(accs)
	weights := make([]int64, len(accs))
	var total int64
	for i, a := range accs {
		wi := int64(s.weightOf(a, maxCredits, now)*weightScale + 0.5)
		if wi < 1 {
			wi = 1
		}
		weights[i] = wi
		total += wi
	}
	if total <= 0 {
		return accs[0]
	}
	if rnd == nil {
		rnd = rand.Int63n
	}
	r := rnd(total)
	var acc int64
	for i := range accs {
		acc += weights[i]
		if r < acc {
			return accs[i]
		}
	}
	return accs[len(accs)-1]
}
