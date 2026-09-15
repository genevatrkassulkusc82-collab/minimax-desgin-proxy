package main

import "testing"

// TestParseVideoSizeAlias Sora 风格 size 字段解析（infinite-canvas 兼容垫片）
func TestParseVideoSizeAlias(t *testing.T) {
	tests := []struct {
		size     string
		wantRes  string
		wantRat  string
	}{
		{"1280x720", "768P", "16:9"},
		{"720x1280", "768P", "9:16"},
		{"1024x1024", "768P", "1:1"},
		{"1920x1080", "2K", "16:9"},
		{"2560x1440", "2K", "16:9"},
		{"1536x1024", "768P", "3:2"},
		{"1024x1536", "768P", "2:3"},
		{"2100x900", "2K", "21:9"},
		{"720p", "768P", ""},
		{"1080P", "768P", ""},
		{"480p", "480P", ""},
		{"2k", "2K", ""},
		{"4K", "2K", ""},
		{"auto", "", ""},
		{"", "", ""},
		{"garbage", "", ""},
		{"0x0", "", ""},
	}
	for _, tt := range tests {
		res, rat := parseVideoSizeAlias(tt.size)
		if res != tt.wantRes || rat != tt.wantRat {
			t.Errorf("parseVideoSizeAlias(%q) = (%q,%q), want (%q,%q)", tt.size, res, rat, tt.wantRes, tt.wantRat)
		}
	}
}

// TestSnapVideoRatio 比例吸附（对齐官方对数距离算法）
func TestSnapVideoRatio(t *testing.T) {
	tests := []struct {
		ratio float64
		want  string
	}{
		{16.0 / 9.0, "16:9"},
		{1.5, "3:2"},
		{1.0, "1:1"},
		{0.5625, "9:16"},
		{2.33, "21:9"},
		{0, ""},
		{-1, ""},
	}
	for _, tt := range tests {
		if got := snapVideoRatio(tt.ratio); got != tt.want {
			t.Errorf("snapVideoRatio(%v) = %q, want %q", tt.ratio, got, tt.want)
		}
	}
}

// TestValidateAndNormalizeSecondsAlias seconds/n_seconds 别名与显式 duration 的优先级
func TestValidateAndNormalizeSecondsAlias(t *testing.T) {
	sec := flexInt(7)
	nsec := flexInt(9)
	five := 5

	// seconds 字符串形态（JSON "7"）经 flexInt 解码
	body := &createVideoBody{Model: "MiniMax-H3", Prompt: "test", Seconds: &sec}
	req, errMsg := validateAndNormalize(body)
	if req == nil {
		t.Fatalf("seconds 别名解析失败: %s", errMsg)
	}
	if req.Duration != 7 {
		t.Errorf("duration = %d, want 7 (来自 seconds)", req.Duration)
	}

	// 显式 duration 优先于 seconds
	body2 := &createVideoBody{Model: "MiniMax-H3", Prompt: "test", Duration: &five, Seconds: &sec}
	req2, _ := validateAndNormalize(body2)
	if req2 == nil || req2.Duration != 5 {
		t.Errorf("显式 duration 应优先, got %+v", req2)
	}

	// n_seconds 兜底
	body3 := &createVideoBody{Model: "MiniMax-H3", Prompt: "test", NSeconds: &nsec}
	req3, _ := validateAndNormalize(body3)
	if req3 == nil || req3.Duration != 9 {
		t.Errorf("n_seconds 别名应生效, got %+v", req3)
	}

	// size 别名 → resolution + ratio
	body4 := &createVideoBody{Model: "MiniMax-H3", Prompt: "test", Size: "1920x1080"}
	req4, _ := validateAndNormalize(body4)
	if req4 == nil || req4.Resolution != "2K" || req4.Ratio != "16:9" {
		t.Errorf("size 别名映射错误: %+v", req4)
	}

	// 显式 resolution 优先于 size
	body5 := &createVideoBody{Model: "MiniMax-H3", Prompt: "test", Size: "1920x1080", Resolution: "768p"}
	req5, _ := validateAndNormalize(body5)
	if req5 == nil || req5.Resolution != "768P" {
		t.Errorf("显式 resolution 应优先: %+v", req5)
	}

	// H3-Max + size 别名 2K → 钳制到 768P（兼容层不报错）
	body6 := &createVideoBody{Model: "MiniMax-H3-Max", Prompt: "test", Size: "1920x1080"}
	req6, errMsg6 := validateAndNormalize(body6)
	if req6 == nil {
		t.Fatalf("H3-Max size 钳制失败: %s", errMsg6)
	}
	if req6.Resolution != "768P" {
		t.Errorf("H3-Max 2K 别名应钳制为 768P, got %q", req6.Resolution)
	}

	// H3-Max + 显式 2K → 仍然严格报错（不破坏原契约）
	res2k := "2K"
	body7 := &createVideoBody{Model: "MiniMax-H3-Max", Prompt: "test", Resolution: res2k}
	if req7, _ := validateAndNormalize(body7); req7 != nil {
		t.Error("H3-Max 显式 2K 应报错拒绝")
	}
}
