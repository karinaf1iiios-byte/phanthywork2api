package upstream

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 账号池身份分配：同账号复用、不同账号独立、installationId 可推导。
func TestPoolPerAccountIsolation(t *testing.T) {
	dir := t.TempDir()
	p := NewPool(dir, DeviceScopeAccount)

	a, err := p.Identity("scope-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Identity("scope-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.InstallationID == b.InstallationID {
		t.Fatal("不同账号不应共享 installationId")
	}
	if a.DeviceSeed == b.DeviceSeed {
		t.Fatal("account 作用域下设备钥匙应各自独立")
	}
	// installationId 必须能从 identity 公钥重算出来（服务端会重算并比对）
	if got := DeriveInstallationID(a.identityKey().Public().(ed25519.PublicKey)); got != a.InstallationID {
		t.Fatalf("installationId 推导不一致: %s != %s", got, a.InstallationID)
	}
	same, err := NewPool(dir, DeviceScopeAccount).Identity("scope-a")
	if err != nil {
		t.Fatal(err)
	}
	if same.InstallationID != a.InstallationID || same.IdentitySeed != a.IdentitySeed {
		t.Fatal("重载后身份应保持不变")
	}
}

// machine 作用域：全池共用一把 device 钥匙，identity/session 仍各自独立。
func TestPoolMachineDeviceKey(t *testing.T) {
	dir := t.TempDir()
	p := NewPool(dir, DeviceScopeMachine)
	a, err := p.Identity("scope-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Identity("scope-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.DeviceSeed != b.DeviceSeed {
		t.Fatal("machine 作用域下 device 钥匙应一致")
	}
	if a.DevicePublic() != b.DevicePublic() {
		t.Fatal("machine 作用域下 device 公钥应一致")
	}
	if a.InstallationID == b.InstallationID {
		t.Fatal("identity 钥匙仍须独立，installationId 不能相同")
	}
	if _, err := os.Stat(filepath.Join(dir, MachineDeviceFile)); err != nil {
		t.Fatalf("应生成 %s: %v", MachineDeviceFile, err)
	}
	// 文件权限：私钥材料不得对其他用户可读
	st, err := os.Stat(filepath.Join(dir, MachineDeviceFile))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("设备钥匙文件权限应为 0600，实际 %o", st.Mode().Perm())
	}
}

// 换机：旧身份备份 + 新 di_，且不并存。
func TestPoolRotate(t *testing.T) {
	dir := t.TempDir()
	p := NewPool(dir, DeviceScopeAccount)
	old, err := p.Identity("scope-r")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := p.Rotate("scope-r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("应生成备份 %s: %v", backup, err)
	}
	fresh, err := p.Identity("scope-r")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.InstallationID == old.InstallationID {
		t.Fatal("换机后 installationId 必须变化")
	}
	if fresh.IdentitySeed == old.IdentitySeed {
		t.Fatal("换机后 identity 私钥必须变化")
	}
	// 目录里只应有一个在用身份 + 一个备份
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "installation-") && !strings.Contains(e.Name(), ".bak-") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("在用身份应只有 1 个，实际 %d", n)
	}
}

// 元信息落盘：注册/领取/观测结果要能在巡检时读回来。
func TestIdentityMetadataRoundtrip(t *testing.T) {
	dir := t.TempDir()
	p := NewPool(dir, DeviceScopeAccount)
	id, err := p.Identity("scope-x")
	if err != nil {
		t.Fatal(err)
	}
	id.RecordRegister("cmxyz", "phanthy_work", "macos", "2.43.0", "active")
	id.RecordClaim("2026-10-28", "granted", 300, 1)
	id.RecordSummary(2600, 1500)
	if err := id.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(id.Path)
	if err != nil {
		t.Fatal(err)
	}
	var back Identity
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ServerID != "cmxyz" || back.LastClaimDate != "2026-10-28" || back.StreakDay != 1 ||
		back.CreditsAvailable != 2600 || back.OnboardingEarned != 1500 {
		t.Fatalf("元信息未正确落盘: %+v", back)
	}
	list, err := p.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AccountScope != "scope-x" {
		t.Fatalf("List 应只返回身份文件: %+v", list)
	}
}
