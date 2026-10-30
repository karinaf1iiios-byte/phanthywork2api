package server

import (
	"os"
	"testing"
)

// 邀请关系只在注册表单提交那一刻建立，看板因此给的是「带码注册」链接而不是补填按钮。
// 这里守住两件事：新 schema 能读出来；早期 invite_flow.py 的驼峰 schema 也能读出来。
func TestReadInviteCandidate(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	// 没有文件 → nil（看板显示「还没有可用邀请人」，不能报错）
	if got := readInviteCandidate(); got != nil {
		t.Fatalf("无文件时应为 nil，得到 %+v", got)
	}

	if err := os.MkdirAll("data", 0o700); err != nil {
		t.Fatal(err)
	}

	// 新 schema（scripts/invite_link.py 写的）
	newSchema := `{"at":"2026-10-28T08:03:03","inviter":"scopeNew","phone":"138****0000",
		"tier":1,"budget":1,"next_reward":8000,"invite_code":"AAAA-BBBB-CC33",
		"share_url":"https://example.test/r/newcode"}`
	if err := os.WriteFile("data/invite_candidate.json", []byte(newSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	inv := readInviteCandidate()
	if inv == nil {
		t.Fatal("新 schema 读不出来")
	}
	if inv.Tier != 1 || inv.Budget != 1 || inv.NextReward != 8000 || inv.InviteCode == "" || inv.ShareURL == "" {
		t.Errorf("新 schema 字段不对: %+v", inv)
	}

	// 旧 schema（invite_flow.py --pick 写的驼峰键），必须仍然可用
	oldSchema := `{"inviter":"scopeOld","inviterTier":0,"inviterBudgetLeft":2,
		"inviterNextReward":5000,"inviteCode":"DDDD-EEEE-FF44",
		"shareUrl":"https://example.test/r/oldcode","hint":"用新手机号打开 registerUrl 注册"}`
	if err := os.WriteFile("data/invite_candidate.json", []byte(oldSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	inv = readInviteCandidate()
	if inv == nil {
		t.Fatal("旧 schema 读不出来（升级后看板会显示「没有可用邀请人」）")
	}
	if inv.Inviter != "scopeOld" || inv.Budget != 2 || inv.NextReward != 5000 ||
		inv.ShareURL != "https://example.test/r/oldcode" {
		t.Errorf("旧 schema 归一化不对: %+v", inv)
	}
	if inv.Note == "" {
		t.Error("note 应有默认值，说明为什么没有补填入口")
	}

	// 脏文件 / 缺链接 → nil，不 panic
	_ = os.WriteFile("data/invite_candidate.json", []byte(`{"inviter":"x"}`), 0o600)
	if got := readInviteCandidate(); got != nil {
		t.Errorf("缺注册链接时应为 nil，得到 %+v", got)
	}
	_ = os.WriteFile("data/invite_candidate.json", []byte(`not json`), 0o600)
	if got := readInviteCandidate(); got != nil {
		t.Errorf("坏 JSON 时应为 nil，得到 %+v", got)
	}
}
