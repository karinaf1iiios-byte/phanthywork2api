// jobs.go：进程内定时任务（claim / verify / sweep 等），由 config.schedule.jobs 驱动。
//
// 为什么放在进程里，而不是只靠 launchd：
//   - launchd 只在 macOS 上、且只在"这台机器"上有效；换到常开的小主机/容器就没了。
//   - onboarding 1500 与邀请补填的窗口很短，任务跑没跑、上次结果是什么，
//     必须在看板上看得见（否则就是你上次遇到的情况：以为配了，其实没跑）。
//
// 两条实现路径：
//   - claim：直接走进程内的 claimOne（原生 API 调用，不依赖 python）
//   - 其他（verify / sweep / bind / onboard）：exec scripts/pool.sh <cmd>，
//     真正的协议逻辑继续留在 python 里，避免两套实现漂移；pool.sh 自带目录锁，
//     所以即使 launchd 也装着，也不会并发跑同一批账号。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// JobConfig 一条定时任务。Hours 是本地时区的小时点，Minute 是分钟。
type JobConfig struct {
	Name    string `json:"name"`
	Cmd     string `json:"cmd"`
	Hours   []int  `json:"hours"`
	Minute  int    `json:"minute"`
	Enabled bool   `json:"enabled"`
}

// jobStatus 是给看板看的运行态（JSON 里直接给前端）。
type jobStatus struct {
	Name     string   `json:"name"`
	Cmd      string   `json:"cmd"`
	Schedule []string `json:"schedule"`
	Enabled  bool     `json:"enabled"`
	Running  bool     `json:"running"`
	LastRun  string   `json:"last_run,omitempty"`
	LastRC   int      `json:"last_rc"`
	LastTail string   `json:"last_tail,omitempty"`
	LastDurS int64    `json:"last_duration_seconds"`
	NextRun  string   `json:"next_run,omitempty"`
}

type jobRunner struct {
	srv    *Server
	cfg    []JobConfig
	mu     sync.Mutex
	state  map[string]*jobStatus
	ranKey map[string]bool // job + "2026-10-28T09"：同一次计划只跑一遍
	busy   bool
}

func newJobRunner(s *Server, cfg []JobConfig) *jobRunner {
	jr := &jobRunner{srv: s, cfg: cfg, state: map[string]*jobStatus{}, ranKey: map[string]bool{}}
	for _, j := range cfg {
		jr.state[j.Name] = &jobStatus{
			Name: j.Name, Cmd: j.Cmd, Enabled: j.Enabled,
			Schedule: jobSchedule(j), LastRC: -1,
		}
	}
	jr.load()
	return jr
}

func jobSchedule(j JobConfig) []string {
	var out []string
	for _, h := range j.Hours {
		out = append(out, fmt.Sprintf("%02d:%02d", h, j.Minute))
	}
	return out
}

// scheduleAt 返回今天该任务的触发时刻列表（升序）。
func scheduleAt(j JobConfig, day time.Time) []time.Time {
	var out []time.Time
	for _, h := range j.Hours {
		out = append(out, time.Date(day.Year(), day.Month(), day.Day(), h, j.Minute, 0, 0, day.Location()))
	}
	return out
}

// dueOccurrence 返回「现在应该跑、但还没跑过」的那一次计划时刻（含休眠后补跑）。
func dueOccurrence(j JobConfig, now time.Time, ran map[string]bool) (time.Time, bool) {
	if !j.Enabled {
		return time.Time{}, false
	}
	var latest time.Time
	for _, at := range scheduleAt(j, now) {
		if now.Before(at) {
			continue
		}
		key := j.Name + "|" + at.Format("2006-01-02T15:04")
		if ran[key] {
			continue
		}
		if at.After(latest) {
			latest = at
		}
	}
	if latest.IsZero() {
		return time.Time{}, false
	}
	return latest, true
}

// nextRun 给看板显示下一次触发时间（今天剩下的或明天的第一个）。
func nextRun(j JobConfig, now time.Time) time.Time {
	var best time.Time
	for d := 0; d < 2; d++ {
		day := now.AddDate(0, 0, d)
		for _, at := range scheduleAt(j, day) {
			if at.After(now) && (best.IsZero() || at.Before(best)) {
				best = at
			}
		}
	}
	return best
}

func (jr *jobRunner) loop() {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for now := range tick.C {
		jr.tick(now)
	}
}

func (jr *jobRunner) tick(now time.Time) {
	for _, j := range jr.cfg {
		at, ok := dueOccurrence(j, now, jr.ranKey)
		if !ok {
			continue
		}
		key := j.Name + "|" + at.Format("2006-01-02T15:04")
		jr.ranKey[key] = true
		jr.snapshotNext()
		go func(j JobConfig, at time.Time) {
			log.Printf("[job] %s 到点（计划 %s），开始执行", j.Name, at.Format("15:04"))
			if rc, _, err := jr.run(j.Name); err != nil {
				log.Printf("[job] %s 失败 rc=%d: %v", j.Name, rc, err)
			} else {
				log.Printf("[job] %s 完成 rc=%d", j.Name, rc)
			}
		}(j, at)
	}
}

// run 执行一条任务（面板的「立即运行」也走这里）。
func (jr *jobRunner) run(name string) (int, string, error) {
	jr.mu.Lock()
	if jr.busy {
		jr.mu.Unlock()
		return -1, "", fmt.Errorf("已有任务在跑")
	}
	st, ok := jr.state[name]
	if !ok {
		jr.mu.Unlock()
		return -1, "", fmt.Errorf("未知任务 %s", name)
	}
	jr.busy = true
	st.Running = true
	var cfg JobConfig
	for _, j := range jr.cfg {
		if j.Name == name {
			cfg = j
		}
	}
	jr.mu.Unlock()

	start := time.Now()
	rc, tail, err := jr.exec(cfg)
	dur := time.Since(start)

	jr.mu.Lock()
	st.Running = false
	st.LastRun = start.Format(time.RFC3339)
	st.LastRC = rc
	st.LastTail = tail
	st.LastDurS = int64(dur.Seconds())
	jr.busy = false
	jr.mu.Unlock()
	jr.save()
	return rc, tail, err
}

func (jr *jobRunner) exec(j JobConfig) (int, string, error) {
	switch strings.TrimSpace(j.Cmd) {
	case "claim":
		return jr.srv.claimAll()
	}

	root := jr.srv.workdir()
	args := strings.Fields(j.Cmd)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{filepath.Join(root, "scripts", "pool.sh")}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	tail := tailText(string(out), 4000)
	rc := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else {
			rc = -1
		}
	}
	return rc, tail, err
}

func tailText(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var keep []string
	for _, l := range lines {
		if strings.Contains(l, "[identity]") {
			continue
		}
		keep = append(keep, l)
	}
	s = strings.Join(keep, "\n")
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

func (jr *jobRunner) snapshotNext() {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	now := time.Now()
	for _, j := range jr.cfg {
		if st := jr.state[j.Name]; st != nil {
			if nx := nextRun(j, now); !nx.IsZero() {
				st.NextRun = nx.Format(time.RFC3339)
			}
		}
	}
}

func (jr *jobRunner) list() []jobStatus {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	now := time.Now()
	out := make([]jobStatus, 0, len(jr.cfg))
	for _, j := range jr.cfg {
		st := jr.state[j.Name]
		if st == nil {
			continue
		}
		cp := *st
		if nx := nextRun(j, now); !nx.IsZero() {
			cp.NextRun = nx.Format(time.RFC3339)
		}
		out = append(out, cp)
	}
	return out
}

// 运行态落盘：重启服务后看板还能看到「上次什么时候跑的、结果如何」。
func (jr *jobRunner) path() string {
	return filepath.Join(jr.srv.workdir(), "data", "jobs.json")
}

func (jr *jobRunner) save() {
	jr.mu.Lock()
	snap := make(map[string]jobStatus, len(jr.state))
	for k, v := range jr.state {
		snap[k] = *v
	}
	jr.mu.Unlock()
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	p := jr.path()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, p)
	}
}

func (jr *jobRunner) load() {
	raw, err := os.ReadFile(jr.path())
	if err != nil {
		return
	}
	var snap map[string]jobStatus
	if err := json.Unmarshal(raw, &snap); err != nil {
		return
	}
	jr.mu.Lock()
	defer jr.mu.Unlock()
	for name, old := range snap {
		if st, ok := jr.state[name]; ok {
			st.LastRun, st.LastRC, st.LastTail, st.LastDurS = old.LastRun, old.LastRC, old.LastTail, old.LastDurS
		}
	}
}
