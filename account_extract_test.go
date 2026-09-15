package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// makeJWT 构造无签名测试 JWT（仅 payload 有效即可，extractDeviceID 不验签）
func makeJWT(payloadJSON string) string {
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	return enc(`{"alg":"HS256","typ":"JWT"}`) + "." + enc(payloadJSON) + ".sig"
}

func TestExtractDeviceID(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		wantID    string
		wantOK    bool
		wantError bool
	}{
		{
			name:   "标准 user.deviceID",
			token:  makeJWT(`{"user":{"deviceID":"dev-abc-123","uid":1}}`),
			wantID: "dev-abc-123", wantOK: true,
		},
		{
			name:   "user.deviceID 为空串（网页端 token）——不报错，ok=false",
			token:  makeJWT(`{"user":{"deviceID":"","uid":1}}`),
			wantID: "", wantOK: false,
		},
		{
			name:   "无 user 字段——不报错，ok=false",
			token:  makeJWT(`{"sub":"123","exp":9999999999}`),
			wantID: "", wantOK: false,
		},
		{
			name:   "变体 user.device_id",
			token:  makeJWT(`{"user":{"device_id":"dev-variant"}}`),
			wantID: "dev-variant", wantOK: true,
		},
		{
			name:   "顶层 deviceID 兜底",
			token:  makeJWT(`{"deviceID":"dev-top"}`),
			wantID: "dev-top", wantOK: true,
		},
		{
			name:   "deviceID 类型异常（数字）——ok=false 不报错",
			token:  makeJWT(`{"user":{"deviceID":12345}}`),
			wantID: "", wantOK: false,
		},
		{
			name:   "payload 非法 base64——报错",
			token:  "eyJhbGci.!!!not-base64!!!.sig",
			wantID: "", wantOK: false, wantError: true,
		},
		{
			name:   "payload 非法 JSON——报错",
			token:  makeJWT2("e30"), // "e30" 解码为 "{}"，改用真正非法的
			wantID: "", wantOK: false, wantError: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "payload 非法 JSON——报错" {
				// 特殊构造：base64 合法但 JSON 非法
				tt.token = "eyJhbGci." + base64.RawURLEncoding.EncodeToString([]byte(`{invalid`)) + ".sig"
			}
			id, ok, err := extractDeviceID(tt.token)
			if tt.wantError {
				if err == nil {
					t.Fatalf("期望报错，实际 nil (id=%q ok=%v)", id, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if ok != tt.wantOK || id != tt.wantID {
				t.Fatalf("got (%q,%v), want (%q,%v)", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

func makeJWT2(payloadB64 string) string {
	return "eyJhbGci." + payloadB64 + ".sig"
}

func TestNewDeviceUUID(t *testing.T) {
	u := newDeviceUUID()
	// 8-4-4-4-12 格式
	parts := strings.Split(u, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID 格式错误: %q", u)
	}
	wantLens := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != wantLens[i] {
			t.Fatalf("第 %d 段长度 %d != %d: %q", i, len(p), wantLens[i], u)
		}
	}
	// version 4 / variant 位
	if parts[2][0] != '4' {
		t.Fatalf("非 UUID v4: %q", u)
	}
	if !strings.ContainsRune("89ab", rune(parts[3][0])) {
		t.Fatalf("variant 位错误: %q", u)
	}
	// 两次生成不重复
	if newDeviceUUID() == u {
		t.Fatal("连续生成的 UUID 重复")
	}
}

func TestDeepLinkScheme(t *testing.T) {
	if got := DeepLinkScheme("domestic"); got != "minimax-hub-cn" {
		t.Errorf("domestic scheme = %q", got)
	}
	if got := DeepLinkScheme("overseas"); got != "minimax-hub" {
		t.Errorf("overseas scheme = %q", got)
	}
	if got := DeepLinkScheme(""); got != "minimax-hub-cn" {
		t.Errorf("空 region 应默认 domestic, got %q", got)
	}
}

func TestBuildRestoreLink(t *testing.T) {
	// 与官方网页端签发 token 同构的 payload（exp + user.id 字符串形态）
	tok := makeJWT(`{"exp":1792828128,"user":{"id":"123456789098765432","name":"","avatar":"","deviceID":"","isAnonymous":false}}`)

	link, exp, userID := BuildRestoreLink("domestic", tok)
	want := "minimax-hub-cn://auth-callback?accessToken=" + tok
	if link != want {
		t.Errorf("link = %q, want %q", link, want)
	}
	if exp != 1792828128 {
		t.Errorf("exp = %d, want 1792828128", exp)
	}
	if userID != "123456789098765432" {
		t.Errorf("userID = %q", userID)
	}

	// 海外 region
	link2, _, _ := BuildRestoreLink("overseas", tok)
	if !strings.HasPrefix(link2, "minimax-hub://auth-callback?accessToken=") {
		t.Errorf("overseas link = %q", link2)
	}

	// exp/user.id 缺失或类型异常：不 panic，返回零值
	link3, exp3, uid3 := BuildRestoreLink("domestic", makeJWT(`{"sub":"x"}`))
	if exp3 != 0 || uid3 != "" || !strings.HasPrefix(link3, "minimax-hub-cn://") {
		t.Errorf("缺失字段处理异常: exp=%d uid=%q link=%q", exp3, uid3, link3)
	}
	// user.id 为数字形态
	_, _, uid4 := BuildRestoreLink("domestic", makeJWT(`{"exp":123,"user":{"id":42}}`))
	if uid4 != "42" {
		t.Errorf("数字 id 应转字符串, got %q", uid4)
	}
	// 非法 JWT：仍生成链接（token 原样拼接），exp/userID 为零值
	link5, exp5, _ := BuildRestoreLink("domestic", "not-a-jwt")
	if exp5 != 0 || link5 != "minimax-hub-cn://auth-callback?accessToken=not-a-jwt" {
		t.Errorf("非法 JWT 处理异常: %q exp=%d", link5, exp5)
	}
}
