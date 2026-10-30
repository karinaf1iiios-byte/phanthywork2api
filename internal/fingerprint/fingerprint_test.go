package fingerprint

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestDeriveIsStable(t *testing.T) {
	seed := []byte("di_test_seed_0123456789")
	a, b := Derive(seed), Derive(seed)
	if a.DeviceID != b.DeviceID || a.OSRelease != b.OSRelease || a.MachineID != b.MachineID {
		t.Fatalf("同种子应完全一致: %+v vs %+v", a, b)
	}
	if len(a.DeviceID) != 32 {
		t.Fatalf("device_id 应为 32 hex，得到 %q", a.DeviceID)
	}
}

func TestDeriveDiffersAcrossSeeds(t *testing.T) {
	a := Derive([]byte("seed-a"))
	b := Derive([]byte("seed-b"))
	if a.DeviceID == b.DeviceID {
		t.Fatal("不同种子不应共用 device_id")
	}
	if len(a.IPCandidates) == 0 || a.IPCandidates[0] == "" {
		t.Fatal("应至少给一个局域网 IP")
	}
	if !strings.HasPrefix(a.IPCandidates[0], "192.168.") && !strings.HasPrefix(a.IPCandidates[0], "10.") {
		t.Fatalf("IP 不像家用网段: %v", a.IPCandidates)
	}
}

func TestUserAgentFormat(t *testing.T) {
	ua := Derive([]byte("x")).UserAgent()
	if !strings.HasPrefix(ua, "claude-cli/") || !strings.Contains(ua, "(external, cli)") {
		t.Fatalf("UA 格式不对: %q", ua)
	}
}

func TestCliDeviceHeader(t *testing.T) {
	p := Derive([]byte("hdr"))
	raw, err := base64.RawURLEncoding.DecodeString(p.CliDeviceHeader())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "platform", "arch", "osRelease", "cliVersion", "ipCandidates"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("cli-device 缺字段 %s: %s", k, raw)
		}
	}
	if doc["version"].(float64) != 1 {
		t.Fatalf("version 应为 1")
	}
	if doc["cliVersion"] != DefaultCLIVersion {
		t.Fatalf("cliVersion=%v", doc["cliVersion"])
	}
}

func TestMetadataUserID(t *testing.T) {
	p := Derive([]byte("md"))
	s := p.MetadataUserID("acct-uuid", "sess-uuid")
	var doc map[string]string
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("user_id 必须是 JSON 串: %v (%s)", err, s)
	}
	if doc["device_id"] != p.DeviceID || doc["account_uuid"] != "acct-uuid" || doc["session_id"] != "sess-uuid" {
		t.Fatalf("字段不对: %s", s)
	}
}

func TestNormalizeFillsMissing(t *testing.T) {
	p := (&Profile{Platform: "win32"}).Normalize()
	if p.Arch == "" || p.OSRelease == "" || p.DeviceID == "" || p.CLIVersion == "" {
		t.Fatalf("补齐失败: %+v", p)
	}
	if p.UserAgent() == "" {
		t.Fatal("UA 为空")
	}
}
