package main

// ---- 图片生成（nano_banana）单元测试 ----
// 覆盖：模型别名归一、size→aspect_ratio 映射、入参校验、后端注册表、
// v2 查询响应双错误包（base / base_resp）宽容解析。

import (
	"encoding/json"
	"testing"
)

func TestNormalizeImageModel(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "", want: ImageModelNanoBanana2Flash, wantOK: true}, // 缺省默认
		{in: "nano_banana_2_flash", want: ImageModelNanoBanana2Flash, wantOK: true},
		{in: "NANO-BANANA-2-FLASH", want: ImageModelNanoBanana2Flash, wantOK: true},
		{in: "general-image-2", want: ImageModelNanoBanana2Flash, wantOK: true},
		{in: "General_Image_2", want: ImageModelNanoBanana2Flash, wantOK: true},
		{in: "gpt-image-nano", want: ImageModelNanoBanana2Flash, wantOK: true},
		{in: "dall-e-3", wantOK: false},
		{in: "MiniMax-H3", wantOK: false}, // 视频模型不属于图片
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := normalizeImageModel(tt.in)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("normalizeImageModel(%q) = (%q,%v), want (%q,%v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestValidateAndNormalizeImage(t *testing.T) {
	t.Run("size 映射 aspect_ratio", func(t *testing.T) {
		cases := map[string]string{
			"1024x1024": "1:1",
			"1536x1024": "3:2",
			"1024x1536": "2:3",
		}
		for size, want := range cases {
			req, n, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Size: size})
			if req == nil {
				t.Fatalf("size=%s 校验失败: %s", size, errMsg)
			}
			if req.AspectRatio != want || n != 1 {
				t.Fatalf("size=%s → aspect=%q n=%d, want %q/1", size, req.AspectRatio, n, want)
			}
		}
	})

	t.Run("aspect_ratio 优先于 size", func(t *testing.T) {
		req, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Size: "1024x1024", AspectRatio: "21:9"})
		if req == nil {
			t.Fatalf("意外失败: %s", errMsg)
		}
		if req.AspectRatio != "21:9" {
			t.Fatalf("aspect_ratio 应优先: %q", req.AspectRatio)
		}
	})

	t.Run("默认值", func(t *testing.T) {
		req, _, _ := validateAndNormalizeImage(&createImageBody{Prompt: "x"})
		if req.Model != ImageModelNanoBanana2Flash || req.Resolution != "1K" ||
			req.AspectRatio != "" || req.ResponseFormat != "url" {
			t.Fatalf("默认值不符: %+v", req)
		}
	})

	t.Run("resolution 档位", func(t *testing.T) {
		for _, res := range []string{"2K", "4k"} {
			req, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Resolution: res})
			if req == nil {
				t.Fatalf("resolution=%s 意外失败: %s", res, errMsg)
			}
		}
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Resolution: "8K"}); errMsg == "" {
			t.Fatal("8K 应被拒绝")
		}
	})

	t.Run("n 范围 1-4", func(t *testing.T) {
		n4 := 4
		if _, n, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", N: &n4}); n != 4 || errMsg != "" {
			t.Fatalf("n=4 应合法: n=%d err=%s", n, errMsg)
		}
		n5 := 5
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", N: &n5}); errMsg == "" {
			t.Fatal("n=5 应被拒绝")
		}
		n0 := 0
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", N: &n0}); errMsg == "" {
			t.Fatal("n=0 应被拒绝")
		}
	})

	t.Run("prompt 必填", func(t *testing.T) {
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "  "}); errMsg == "" {
			t.Fatal("空 prompt 应被拒绝")
		}
	})

	t.Run("参考图上限 10 张", func(t *testing.T) {
		imgs := make([]string, 11)
		for i := range imgs {
			imgs[i] = "https://example.com/x.png"
		}
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Images: imgs}); errMsg == "" {
			t.Fatal("11 张参考图应被拒绝")
		}
		imgs = imgs[:10]
		if req, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Images: imgs}); req == nil || len(req.Images) != 10 {
			t.Fatalf("10 张应合法: %s", errMsg)
		}
	})

	t.Run("参考图协议校验", func(t *testing.T) {
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Image: "ftp://a/b.png"}); errMsg == "" {
			t.Fatal("ftp 协议应被拒绝")
		}
		req, _, _ := validateAndNormalizeImage(&createImageBody{
			Prompt: "x", Image: "https://a/1.png", Images: []string{"data:image/png;base64,AAA"},
		})
		if len(req.Images) != 2 || req.Images[0] != "https://a/1.png" {
			t.Fatalf("image 应合并进 images 首位: %+v", req.Images)
		}
	})

	t.Run("response_format", func(t *testing.T) {
		req, _, _ := validateAndNormalizeImage(&createImageBody{Prompt: "x", ResponseFormat: "b64_json"})
		if req.ResponseFormat != "b64_json" {
			t.Fatalf("got %q", req.ResponseFormat)
		}
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", ResponseFormat: "pdf"}); errMsg == "" {
			t.Fatal("非法 response_format 应被拒绝")
		}
	})

	t.Run("非法模型/比例/size", func(t *testing.T) {
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Model: "midjourney-v7", Prompt: "x"}); errMsg == "" {
			t.Fatal("非法模型应被拒绝")
		}
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", AspectRatio: "7:3"}); errMsg == "" {
			t.Fatal("非法 aspect_ratio 应被拒绝")
		}
		if _, _, errMsg := validateAndNormalizeImage(&createImageBody{Prompt: "x", Size: "999x999"}); errMsg == "" {
			t.Fatal("非法 size 应被拒绝")
		}
	})
}

func TestImageBackendForModel(t *testing.T) {
	b, ok := ImageBackendForModel("nano_banana_2_flash")
	if !ok {
		t.Fatal("nano_banana_2_flash 应命中注册表")
	}
	if b.GenerateV2 != "/api/v2/image/nano_banana/generate" || b.QueryV2 != "/api/v2/image/nano_banana/tasks" {
		t.Fatalf("路径不符: %+v", b)
	}
	if b.Generate != "/api/v1/image/nano_banana/generate" {
		t.Fatalf("v1 同步路径不符: %+v", b)
	}
	if _, ok := ImageBackendForModel("seedream_4"); ok {
		t.Fatal("未注册厂商不应命中（本期只做 nano_banana）")
	}
}

func TestCloudImageStatusParsing(t *testing.T) {
	t.Run("success 数字字段宽容", func(t *testing.T) {
		var st CloudImageStatus
		raw := `{"status":"success","image_url":"https://cdn.example/x.png","width":"1024","height":1024}`
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if st.Status != "success" || st.ImageURL == "" || int(st.Width) != 1024 || int(st.Height) != 1024 {
			t.Fatalf("字段不符: %+v", st)
		}
	})

	t.Run("failed 用 base 包（v2 图片语义）", func(t *testing.T) {
		var st CloudImageStatus
		raw := `{"status":"failed","base":{"code":"1033","message":"busy now","user_message":"稍后再试"}}`
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if st.ErrCode() != 1033 || st.ErrMessage() != "busy now" {
			t.Fatalf("base 包解析不符: code=%d msg=%q", st.ErrCode(), st.ErrMessage())
		}
	})

	t.Run("failed 用 base_resp 包（双容忍）", func(t *testing.T) {
		var st CloudImageStatus
		raw := `{"status":"failed","base_resp":{"status_code":2013,"status_msg":"param error"}}`
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if st.ErrCode() != 2013 || st.ErrMessage() != "param error" {
			t.Fatalf("base_resp 包解析不符: code=%d msg=%q", st.ErrCode(), st.ErrMessage())
		}
	})

	t.Run("processing 无错误包", func(t *testing.T) {
		var st CloudImageStatus
		if err := json.Unmarshal([]byte(`{"status":"processing"}`), &st); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if st.ErrCode() != 0 {
			t.Fatalf("进行中不应有错误码: %d", st.ErrCode())
		}
	})
}
