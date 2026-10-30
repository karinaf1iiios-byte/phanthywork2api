package server

import (
	"fmt"
	"testing"
	"time"
)

func at(h, m int) time.Time {
	return time.Date(2026, 10, 28, h, m, 0, 0, time.Local)
}

func TestDueOccurrence(t *testing.T) {
	job := JobConfig{Name: "claim", Cmd: "claim", Hours: []int{9, 21}, Minute: 0, Enabled: true}
	ran := map[string]bool{}

	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"差一分钟不跑", at(8, 59), false},
		{"到点就跑", at(9, 0), true},
		{"中间不重复（无 ran 标记时仍算今天 9:00 那次）", at(12, 0), true},
	}
	for _, c := range cases {
		if _, ok := dueOccurrence(job, c.now, map[string]bool{}); ok != c.want {
			t.Errorf("%s: dueOccurrence(%s)=%v want %v", c.name, c.now.Format("15:04"), ok, c.want)
		}
	}

	// 跑过之后同一次计划不再触发；21:00 那次仍然会触发
	first, ok := dueOccurrence(job, at(9, 0), ran)
	if !ok {
		t.Fatal("9:00 应触发")
	}
	ran["claim|"+first.Format("2006-01-02T15:04")] = true
	if _, ok := dueOccurrence(job, at(9, 20), ran); ok {
		t.Error("同一次计划不应重复触发")
	}
	second, ok := dueOccurrence(job, at(21, 1), ran)
	if !ok || second.Hour() != 21 {
		t.Errorf("21:00 应触发，got %v ok=%v", second, ok)
	}
}

// 休眠错过整点后要能补跑（launchd 的日历任务也有这个行为）。
func TestDueOccurrenceCatchUpAfterSleep(t *testing.T) {
	job := JobConfig{Name: "sweep", Cmd: "sweep --apply", Hours: []int{9, 21}, Minute: 40, Enabled: true}
	occ, ok := dueOccurrence(job, at(23, 30), map[string]bool{})
	if !ok {
		t.Fatal("23:30 应补跑 21:40 那次")
	}
	if occ.Hour() != 21 || occ.Minute() != 40 {
		t.Errorf("补跑的应是 21:40，got %s", occ.Format("15:04"))
	}
}

func TestDueOccurrenceDisabled(t *testing.T) {
	job := JobConfig{Name: "sweep", Hours: []int{9}, Minute: 40, Enabled: false}
	if _, ok := dueOccurrence(job, at(10, 0), map[string]bool{}); ok {
		t.Error("enabled=false 不应触发")
	}
}

func TestNextRun(t *testing.T) {
	job := JobConfig{Name: "claim", Hours: []int{9, 21}, Minute: 0, Enabled: true}
	if nx := nextRun(job, at(8, 0)); nx.Hour() != 9 || nx.Day() != 28 {
		t.Errorf("08:00 的下一次应是当天 09:00，got %s", nx.Format("01-02 15:04"))
	}
	if nx := nextRun(job, at(22, 0)); nx.Hour() != 9 || nx.Day() != 29 {
		t.Errorf("22:00 的下一次应是次日 09:00，got %s", nx.Format("01-02 15:04"))
	}
}

func TestJobScheduleLabel(t *testing.T) {
	got := jobSchedule(JobConfig{Hours: []int{9, 21}, Minute: 40})
	if len(got) != 2 || got[0] != "09:40" || got[1] != "21:40" {
		t.Errorf("jobSchedule=%v want [09:40 21:40]", got)
	}
}

// 默认配置必须把四条任务都开着（「config 里要把任务都打开」的回归保护）。
// invite 也是常规项：邀请关系只能在注册表单建立，链接要一直有新鲜的。
func TestDefaultConfigJobsEnabled(t *testing.T) {
	cfg := DefaultConfig()
	want := map[string]bool{"invite": false, "claim": false, "verify": false, "sweep": false}
	for _, j := range cfg.Schedule.Jobs {
		if _, ok := want[j.Name]; !ok {
			t.Errorf("默认任务里有未知项 %s", j.Name)
			continue
		}
		want[j.Name] = j.Enabled
		if len(j.Hours) == 0 {
			t.Errorf("%s 没有配置小时点", j.Name)
		}
	}
	for name, on := range want {
		if !on {
			t.Errorf("默认配置里 %s 未启用", name)
		}
	}
	// 任务时间不能重叠（用户明确要求），同小时同分钟算冲突
	seen := map[string]string{}
	for _, j := range cfg.Schedule.Jobs {
		for _, h := range j.Hours {
			k := fmt.Sprintf("%02d:%02d", h, j.Minute)
			if prev, dup := seen[k]; dup {
				t.Errorf("%s 与 %s 撞在同一时刻 %s", prev, j.Name, k)
			}
			seen[k] = j.Name
		}
	}
}
