package main

// ---- flexFloat / flexInt 宽容数字解析单元测试 ----
// 背景：云端数字字段的 string/number 形态不稳定（实测 credit/balance 的
// total_credit 返回字符串 "1234"），官方网关 asNonNegativeCredits / numberField2
// 均做双形态兼容，客户端必须同样宽容解析。

import (
	"encoding/json"
	"testing"
)

func TestFlexFloatUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    float64
		wantErr bool
	}{
		{name: "JSON number 整数", in: `1234`, want: 1234},
		{name: "JSON number 小数", in: `12.5`, want: 12.5},
		{name: "字符串整数（实测 total_credit 形态）", in: `"1234"`, want: 1234},
		{name: "字符串小数", in: `"12.5"`, want: 12.5},
		{name: "字符串科学计数法", in: `"1.5e2"`, want: 150},
		{name: "负数", in: `-3.5`, want: -3.5},
		{name: "字符串负数", in: `"-3.5"`, want: -3.5},
		{name: "字符串带空白", in: `" 42 "`, want: 42},
		{name: "零", in: `0`, want: 0},
		{name: "字符串零", in: `"0"`, want: 0},
		{name: "空串按 0", in: `""`, want: 0},
		{name: "null 按 0", in: `null`, want: 0},
		{name: "非法字符串报错", in: `"abc"`, wantErr: true},
		{name: "NaN 字符串报错", in: `"NaN"`, wantErr: true},
		{name: "Inf 字符串报错", in: `"Inf"`, wantErr: true},
		{name: "布尔报错", in: `true`, wantErr: true},
		{name: "对象报错", in: `{}`, wantErr: true},
		{name: "数组报错", in: `[1]`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f flexFloat
			err := json.Unmarshal([]byte(tt.in), &f)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %v", float64(f))
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if float64(f) != tt.want {
				t.Fatalf("got %v, want %v", float64(f), tt.want)
			}
		})
	}
}

func TestFlexIntUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{name: "JSON number", in: `1033`, want: 1033},
		{name: "字符串整数", in: `"1033"`, want: 1033},
		{name: "小数向零截断", in: `12.7`, want: 12},
		{name: "字符串小数向零截断", in: `"12.5"`, want: 12},
		{name: "负小数向零截断", in: `-12.5`, want: -12},
		{name: "零", in: `0`, want: 0},
		{name: "空串按 0", in: `""`, want: 0},
		{name: "null 按 0", in: `null`, want: 0},
		{name: "非法字符串报错", in: `"12ab"`, wantErr: true},
		{name: "布尔报错", in: `false`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var i flexInt
			err := json.Unmarshal([]byte(tt.in), &i)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %v", int(i))
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if int(i) != tt.want {
				t.Fatalf("got %v, want %v", int(i), tt.want)
			}
		})
	}
}

// TestFlexMarshalIsNumber 编码仍输出 JSON number（对前端/下游契约不变）
func TestFlexMarshalIsNumber(t *testing.T) {
	b, err := json.Marshal(struct {
		F flexFloat `json:"f"`
		I flexInt   `json:"i"`
	}{F: 12.5, I: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"f":12.5,"i":7}` {
		t.Fatalf("编码结果不是 number 形态: %s", b)
	}
}

// TestFlexBalanceResponse 复现实测故障：credit/balance 返回字符串 total_credit
func TestFlexBalanceResponse(t *testing.T) {
	var out struct {
		TotalCredit flexFloat `json:"total_credit"`
	}
	// 实测故障形态：字符串
	if err := json.Unmarshal([]byte(`{"total_credit":"1234"}`), &out); err != nil {
		t.Fatalf("字符串 total_credit 解析失败: %v", err)
	}
	if float64(out.TotalCredit) != 1234 {
		t.Fatalf("got %v, want 1234", float64(out.TotalCredit))
	}
	// 原 number 形态不受影响
	if err := json.Unmarshal([]byte(`{"total_credit":88.5}`), &out); err != nil {
		t.Fatalf("number total_credit 解析失败: %v", err)
	}
	if float64(out.TotalCredit) != 88.5 {
		t.Fatalf("got %v, want 88.5", float64(out.TotalCredit))
	}
	// 字段缺失 → 保持零值不报错
	out.TotalCredit = 0
	if err := json.Unmarshal([]byte(`{}`), &out); err != nil {
		t.Fatalf("缺失字段不应报错: %v", err)
	}
}

// TestFlexBaseRespAndTrial 字符串形态的 base_resp.status_code 与 trial snake_case 归一化
func TestFlexBaseRespAndTrial(t *testing.T) {
	var st CloudTaskStatus
	raw := `{"status":"processing","estimated_remaining_wait_seconds":"45",
	         "base_resp":{"status_code":"0","status_msg":"success"}}`
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("字符串形态任务查询响应解析失败: %v", err)
	}
	if int(st.EstRemainingSec) != 45 || int(st.BaseResp.StatusCode) != 0 {
		t.Fatalf("got remaining=%v code=%v, want 45/0", st.EstRemainingSec, st.BaseResp.StatusCode)
	}

	// trial：云端返回 snake_case（free_count 为字符串），归一化为 camelCase 输出 + 宽容数字
	var raw2 map[string]any
	trialRaw := `{"claimed":true,"claimable":false,"free_count":"3","remaining_count":2,
	              "activity_active":true,"claim_hint":"hello"}`
	if err := json.Unmarshal([]byte(trialRaw), &raw2); err != nil {
		t.Fatalf("解析 raw 失败: %v", err)
	}
	trial := fillTrialFromRaw(raw2)
	if trial.FreeCount != 3 || trial.RemainingCount != 2 || !trial.Claimed ||
		trial.ClaimHint != "hello" || !trial.ActivityActive {
		t.Fatalf("trial 归一化不符: %+v", trial)
	}
	// 序列化到前端应为 camelCase（对齐接口契约与管理前端）
	b, _ := json.Marshal(trial)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["freeCount"]; !ok {
		t.Fatalf("输出应为 camelCase freeCount，实际: %s", b)
	}
	if _, ok := m["free_count"]; ok {
		t.Fatalf("输出不应含 snake_case: %s", b)
	}
}
