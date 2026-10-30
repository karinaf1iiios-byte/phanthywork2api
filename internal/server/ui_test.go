package server

import (
	"net/http/httptest"
	"testing"
)

// 用户可能整段粘贴成功页 URL / code#state / 只粘 code，三种都要能解析出来。
func TestCleanCode(t *testing.T) {
	cases := map[string]string{
		"abc123":           "abc123",
		"abc123#state-xyz": "abc123",
		"https://code.phanthy.com/oauth/code/success?code=abc123&state=xyz": "abc123",
		"code=abc123": "abc123",
		"  abc123  ":  "abc123",
		"":            "",
	}
	for in, want := range cases {
		if got := cleanCode(in); got != want {
			t.Errorf("cleanCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskUID(t *testing.T) {
	// 用假 uid：真实账号名会被 publish.sh 的脱敏表替换掉，测试就跟着红了
	if got := maskUID("phanthy-1700000000"); got != "…0000" {
		t.Fatalf("maskUID = %q", got)
	}
	if got := maskUID("abc"); got != "abc" {
		t.Fatalf("短 uid 不该被截断: %q", got)
	}
}

// 默认只放行本机/内网；配了 ui.access_token 才认 token。
func TestUIAllowed(t *testing.T) {
	s := &Server{cfg: DefaultConfig()}
	mk := func(remote, token, header string) bool {
		r := httptest.NewRequest("GET", "/ui/"+token, nil)
		r.RemoteAddr = remote
		if header != "" {
			r.Header.Set("X-UI-Token", header)
		}
		return s.uiAllowed(r)
	}
	if !mk("127.0.0.1:5000", "", "") {
		t.Fatal("回环地址应放行")
	}
	if !mk("192.168.1.20:5000", "", "") {
		t.Fatal("内网地址应放行")
	}
	if mk("8.8.8.8:5000", "", "") {
		t.Fatal("公网地址默认应拒绝")
	}

	s.cfg.UI.AccessToken = "s3cret"
	if mk("8.8.8.8:5000", "?token=s3cret", "") != true {
		t.Fatal("配了 access_token 后带 ?token= 应放行")
	}
	if !mk("8.8.8.8:5000", "", "s3cret") {
		t.Fatal("带 X-UI-Token 头应放行")
	}
	if mk("8.8.8.8:5000", "?token=wrong", "") {
		t.Fatal("错的 token 应拒绝")
	}
}
