package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeToken(sub string) string {
	payload, _ := json.Marshal(map[string]any{"sub": sub, "exp": 4102444800})
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return "eyJhbGciOiJub25lIn0." + enc + ".sig"
}

func TestJWTSub(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"正常", fakeToken("cmabc123"), "cmabc123"},
		{"空串", "", ""},
		{"非 JWT", "not-a-token", ""},
		{"两段", "a.b", ""},
	} {
		if got := JWTSub(tc.in); got != tc.want {
			t.Errorf("%s: JWTSub=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestPhoneFromEmail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"+86 138****0000", "138****0000"},
		{"+86138****0000", "138****0000"},
		{"138****0000", "138****0000"},
		{"  +86 139****1234  ", "139****1234"},
		{"someone@example.com", "someone@example.com"}, // 非手机号原样返回
	} {
		if got := PhoneFromEmail(tc.in); got != tc.want {
			t.Errorf("PhoneFromEmail(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// 同一个 accountScope 重复登录只能有一个文件；重复的进 .trash。
func TestSaveLoginDedupesByScope(t *testing.T) {
	dir := t.TempDir()
	scope := "cmSCOPE0001"
	tok := fakeToken(scope)

	p1, dup1, err := SaveLogin(dir, scope, tok, "r1", "Bearer", "", "138****0000", 4102444800)
	if err != nil {
		t.Fatal(err)
	}
	if dup1 != 0 {
		t.Fatalf("首次登录不该有重复，got %d", dup1)
	}
	if want := filepath.Join(dir, "phanthy-"+scope+".json"); p1 != want {
		t.Fatalf("路径应为 %s，实际 %s", want, p1)
	}

	// 第二次登录（refresh 轮换）：覆盖同一个文件，不新建
	tok2 := fakeToken(scope)
	p2, dup2, err := SaveLogin(dir, scope, tok2, "r2", "Bearer", "", "138****0000", 4102444800)
	if err != nil {
		t.Fatal(err)
	}
	if p2 != p1 {
		t.Fatalf("应覆盖同一文件：%s vs %s", p2, p1)
	}
	if dup2 != 0 {
		t.Fatalf("覆盖不应计为合并，got %d", dup2)
	}
	files := authFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("同号只能一个凭证文件，got %v", files)
	}
	// refreshToken 必须被新值覆盖
	var doc map[string]any
	raw, _ := os.ReadFile(p1)
	_ = json.Unmarshal(raw, &doc)
	if a := doc["auth"].(map[string]any); a["refreshToken"] != "r2" {
		t.Fatalf("refreshToken 未更新: %v", a["refreshToken"])
	}
}

// 历史遗留的重复文件（文件名不同、scope 相同）要被合并，多余的进 .trash。
func TestSaveLoginMergesLegacyDuplicates(t *testing.T) {
	dir := t.TempDir()
	tok := fakeToken("cmLEGACY")
	for _, stem := range []string{"phanthy-1700000001", "phanthy-1700000002"} {
		if _, err := SaveToken(dir, stem, tok, "r0", "Bearer", "", stem, "old", 4102444800); err != nil {
			t.Fatal(err)
		}
	}
	before := authFiles(t, dir)
	if len(before) != 2 {
		t.Fatalf("前置条件：应有 2 个文件，got %v", before)
	}

	p, dup, err := SaveLogin(dir, "cmLEGACY", fakeToken("cmLEGACY"), "r1", "Bearer", "", "138****0000", 4102444800)
	if err != nil {
		t.Fatal(err)
	}
	if dup != 1 {
		t.Fatalf("应合并 1 个，got %d", dup)
	}
	if after := authFiles(t, dir); len(after) != 1 {
		t.Fatalf("合并后应剩 1 个，got %v", after)
	}
	if filepath.Base(p) != "phanthy-1700000001.json" {
		t.Fatalf("应保留第一个文件名（粘性/装机身份对齐），got %s", p)
	}
	trash, _ := os.ReadDir(filepath.Join(dir, ".trash"))
	if len(trash) != 1 {
		t.Fatalf(".trash 里应有 1 个备份，got %d", len(trash))
	}
}

// scope 未知时不能把不同账号合并（兜底走时间戳名）。
func TestSaveLoginEmptyScopeDoesNotMerge(t *testing.T) {
	dir := t.TempDir()
	a, _, err := SaveLogin(dir, "", "tok-a", "r", "Bearer", "", "x", 4102444800)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := SaveLogin(dir, "", "tok-b", "r", "Bearer", "", "y", 4102444800)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("scope 为空时两次登录不该写同一文件：%s", a)
	}
}

func authFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "phanthy-") && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	return out
}

// 启动期去重：同一 scope 的多个文件只留最新，其余进 .trash。
func TestDedupeDirKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	scope := "cmDUP"
	old, _ := SaveToken(dir, "phanthy-1700000001", fakeToken(scope), "r0", "Bearer", "", "u1", "138****0000", 4102444800)
	time.Sleep(20 * time.Millisecond)
	newer, _ := SaveToken(dir, "phanthy-1700000002", fakeToken(scope), "r1", "Bearer", "", "u2", "138****0000", 4102444800)
	// 另一个不同号，不能被误伤
	other, _ := SaveToken(dir, "phanthy-1700000003", fakeToken("cmOTHER"), "r0", "Bearer", "", "u3", "139****1234", 4102444800)

	n, err := DedupeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应合并 1 个，got %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("旧文件应被移走: %s", old)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Errorf("最新的应保留: %s", newer)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("不同号不能被误伤: %s", other)
	}
	// 幂等：再跑一次没有可合并的
	if n2, _ := DedupeDir(dir); n2 != 0 {
		t.Errorf("第二次应为 0，got %d", n2)
	}
}
