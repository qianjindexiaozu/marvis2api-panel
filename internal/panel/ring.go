// Ring 日志环形缓冲：镜像 log 输出，供面板 /panel/api/logs 拉取。
// 每条记录带时间与频道标记（chat / probe / sys，按行内容启发式分类）。
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// Entry 一条日志。
type Entry struct {
	Time    time.Time `json:"time"`
	Channel string    `json:"channel"` // chat / probe / sys
	Text    string    `json:"text"`
}

// Ring 固定容量环形缓冲（写满覆盖最旧）。
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	cap  int
	drop int // 已被覆盖淘汰的条数（诊断用）
}

// NewRing 构建容量 cap 的环形缓冲（cap<=0 时按 1）。
func NewRing(cap int) *Ring {
	if cap <= 0 {
		cap = 1
	}
	return &Ring{buf: make([]Entry, 0, cap), cap: cap}
}

// logPrefixRe log 包输出的行首时间戳（Ring 已单独记录 Entry.Time，正文剥去避免重复）。
var logPrefix = func() *regexp.Regexp { return regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `) }()

// Write 实现 io.Writer（log.SetOutput 的写入点）。
// log 包每行一次 Write（带尾部 \n），按行拆分入库。
func (r *Ring) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	if s == "" {
		return len(p), nil
	}
	s = logPrefix.ReplaceAllString(s, "")
	r.mu.Lock()
	r.buf = append(r.buf, Entry{Time: time.Now(), Channel: channelOf(s), Text: s})
	if len(r.buf) > r.cap {
		r.drop += len(r.buf) - r.cap
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
	r.mu.Unlock()
	return len(p), nil
}

// Snapshot 返回时间升序快照（含频道）。
func (r *Ring) Snapshot() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.buf))
	copy(out, r.buf)
	return out
}

// channelOf 启发式频道分类：chat 请求行（表格行 "| #NNN |" 或拒绝日志）/
// probe 探测行 / 其余归 sys。
func channelOf(line string) string {
	switch {
	case strings.HasPrefix(line, "| #"),
		strings.Contains(line, "chat status="),
		strings.Contains(line, "[chat] "):
		return "chat"
	case strings.HasPrefix(line, "probe "), strings.HasPrefix(line, "[scheduler]"):
		return "probe"
	default:
		return "sys"
	}
}
