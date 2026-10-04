// Package logfmt 请求日志的统一文案：label 拼接与逐请求一行表格日志。
//
// 面板日志区镜像了 stdout 的 chat 表格行，本包是两条输出唯一的格式权威，
// 避免控制台与面板各自 format 漂移。
package logfmt

import (
	"fmt"
	"strings"
	"time"
)

// Label 拼接账号 ID 与名称的展示标签："name(id)"；名称为空只出 id，
// id 为空只出 name，两者都空出 "-"。用于日志与面板状态里的人读形态。
func Label(id, name string) string {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	switch {
	case id == "" && name == "":
		return "-"
	case name == "":
		return id
	case id == "":
		return name
	default:
		return name + "(" + id + ")"
	}
}

// Truncate 截断字符串到 n 字节（超限追加省略号）。
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// chatLineState 单请求累加的表格行状态（main 侧接序号）。
type lineOpts struct {
	seq int64
}

// ChatLine 逐请求一行表格日志（对齐 workbuddy2api 的 logChatRow 口径）：
//
//	| #001 | 18:31:31 | deepseek-v4 | stream | 200 | 小号(acct-1) | TTFB=801ms | tok=456 | 174.8tok/s | total=2.6s |
//
// seq 为进程级请求序号（0 时省略该段，测试友好）；completionTokens <0 表示
// usage 缺失 → 显示 "-"；ttfb 仅流式有意义，非流式传 0 显示 "-"。
func ChatLine(seq int64, status int, uid, name, model string, ttfb, total time.Duration, completionTokens int, stream bool) string {
	var b strings.Builder
	b.WriteString("| ")
	if seq > 0 {
		fmt.Fprintf(&b, "#%03d | ", seq)
	}
	b.WriteString(time.Now().Format("15:04:05"))
	b.WriteString(" | ")
	b.WriteString(field(model, 16))
	b.WriteString(" | ")
	if stream {
		b.WriteString("stream")
	} else {
		b.WriteString("sync")
	}
	fmt.Fprintf(&b, " | %d | %s | TTFB=%s", status, Label(uid, name), durOrDash(ttfb))
	if completionTokens >= 0 {
		fmt.Fprintf(&b, " | tok=%d", completionTokens)
		if total > 0 {
			fmt.Fprintf(&b, " | %.1ftok/s", float64(completionTokens)/total.Seconds())
		}
	} else {
		b.WriteString(" | tok=-")
	}
	fmt.Fprintf(&b, " | total=%s |", durOrDash(total))
	return b.String()
}

// field 模型名等展示字段：超宽截断（表格不破行）。
func field(s string, w int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	if len(s) > w {
		return s[:w]
	}
	return s
}

// durOrDash 时长的人读形态：零值显示 "-"（流式 TTFB 之外的字段兜底）。
func durOrDash(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return dur(d)
}

// dur 时长的人读形态：毫秒级用 ms，秒级保留两位小数。
func dur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
