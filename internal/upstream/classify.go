// 错误分类（Classify）：按 HTTP 状态码 + body 关键词把上游错误归入 ErrKind，
// 驱动 pool 冷却状态机。判定顺序自「严」到「宽」，每层的先后都有语义依据。
package upstream

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardQuota                     // 配额耗尽（402 或 body 关键词）→ 硬冷却至配额重置
	ErrSoftRate                      // 429 软限流 → 短冷却（有界退避）
	ErrAuthDead                      // 401/403 认证失效 → 硬冷却（人工修 token）
	ErrNotFound                      // 404 上游偶发 → 固定短冷却，不累计熔断（防雪崩）
	ErrServer                        // 5xx 上游故障 → 熔断计数
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号
	ErrPromptTooLong                 // 上下文超限（请求级错误）→ 不罚账号、不轮转，透传原文
	ErrClient                        // 其他 4xx / 业务错误
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardQuota:
		return "hard_quota"
	case ErrSoftRate:
		return "soft_rate"
	case ErrAuthDead:
		return "auth_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// RetryAfter 上游明示的等待时长（Retry-After 等头解析，见 ParseRetryAfter）。
	// 零值 = 上游未明示，冷却时长回落调用方计算值。挂载点选在 Error 信封：
	// Kind 决定「罚不罚」，RetryAfter 决定「罚多久」，同为上游响应的一等公民。
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// asUpstreamError 提取 *Error（供调用方 errors.As 判别）。
func asUpstreamError(err error) (*Error, bool) {
	ue, ok := err.(*Error)
	return ue, ok
}

// AsUpstreamError 导出版本的错误提取（gateway / scheduler 用）。
func AsUpstreamError(err error) (*Error, bool) { return asUpstreamError(err) }

// hardQuotaMarkers 配额耗尽关键词（小写比较 + 中文原文比较双通道）。
// 上游在状态码非 402 时也会返回配额语义文案，此类响应若不识别，
// 账号既不被冷却也不喂熔断，下次请求仍会被选中。
var hardQuotaMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "quota not enough", "payment required", "out of quota",
	"usage limit reached", "daily limit reached", "token limit",
	"积分不足", "额度不足", "余额不足", "配额不足", "额度用尽", "配额用完", "用量已达上限",
}

// softRateMarkers 限流/节流关键词（小写比较 + 中文原文比较双通道）。
// 词表按子串匹配，宁缺毋滥：只收录明确指向「请求速率被节流」的措辞。
var softRateMarkers = []string{
	"rate limit",    // rate limit / rate limits / rate limiting
	"rate-limiting", // 连字符形态需单列——Contains 不跨 '-'
	"rate-limited",
	"too many requests",
	"请求过于频繁", "限流", "请求太快",
}

// contentBlockedMarkers 内容策略拦截关键词（大小写不敏感子串匹配）。
// 上游内容审核误杀合法流量时返回 400 + 以下文案。这是「误报」，非账号问题——
// ErrContentBlocked 在调用方 applyErrorPolicy 中不罚账号。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"content policy",
	"content filter",
	"sensitive content",
	"内容安全", "敏感内容", "违规内容",
}

// promptTooLongMarkers 上下文超限的**文案**形态（请求级错误：上下文超限是
// 请求的问题不是账号的问题——同一个 body 换任何账号发都会超限，与账号无关）。
var promptTooLongMarkers = []string{
	"prompt is too long",
	"context length exceeded",
	"maximum context length",
	"input too long",
	"上下文过长", "上下文超限", "prompt 过长",
}

// sessionDeadMarkers 会话/token 彻底失效的确定性文案（401 之上的终态）。
// 命中即 ErrAuthDead（同处置：硬冷却人工介入），单列只为日志可读性。
var sessionDeadMarkers = []string{
	"token expired", "token invalid", "invalid token", "unauthorized",
	"session expired", "session not found",
	"登录已过期", "凭证失效",
}

// retryAfterHeaderCandidates 冷却时长优先解析的响应头候选序列：
// retry-after（秒，RFC 7231）/ retry-after-ms（毫秒）/ x-ratelimit-reset
// （epoch 秒或毫秒，取 now+ 剩余量）。大小写不敏感（http.Header.Get 已归一）。
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity 解析结果的上限（超过视为上游异常值丢弃，回落本地计算），
// 与 pool 的 softRateMax 默认 2h 同量级。
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter 从限流/拦截响应头解析上游明示的等待时长：
// 依次尝试 Retry-After（整数秒）→ retry-after-ms（整数毫秒）→
// x-ratelimit-reset（纯数字按 epoch 秒/毫秒推断；HTTP-Date 形态不支持——
// 上游族实践发的是数字）。任一头缺失/非法/非正/超上限则尝试下一头；
// 全部不可用返回 false（调用方回落既有计算值，绝不臆造等待时长）。
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue // 非纯数字（如 HTTP-Date）不解析，宁缺毋滥
		}
		d, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if d <= 0 || d > retryAfterSanity {
			continue // 非正/异常大：丢弃（回落本地计算）
		}
		return d, true
	}
	return 0, false
}

// isAllDigits 报告 s 是否为纯数字（前置快筛，免 strconv 之后再判语义）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRetryNumber 按头名口径把纯数字串折算成时长。x-ratelimit-reset 是
// epoch 时刻而非时长：秒口径（10 位）与毫秒口径（13 位）都按「now+ 该时刻
// 的剩余量」折算，已在过去则不可用。位数不足（8 位以下）无法判定 epoch
// 语义的丢弃（宁缺毋滥：短串多半是序号之类的误用头）。
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	// 上限 16 位防 int64 溢出（超过 epoch 毫秒的现实量级必非法）。
	if len(v) > 16 {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	switch headerName {
	case "Retry-After":
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → 剩余量
		sec := n
		if len(v) >= 12 { // 毫秒口径（13 位）；11 位边界按秒（误判代价是多算 1000 倍）
			sec = n / 1000
		}
		remain := time.Until(time.Unix(sec, 0))
		return remain, true
	}
}

// 定位：重置时间解析正则（上游限流文案带「将在 … 重置」/ "reset at ..." 形态时，
// 冷却到该墙钟而不是指数退避）。预编译为包级 var：错误风暴（429 轰炸）时
// 每个限流 body 都会调用，函数体内 MustCompile 是纯浪费。
var (
	reResetCN = regexp.MustCompile(`将在 (.+?) 重置`)
	reResetEN = regexp.MustCompile(`(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
)

// resetTimeLayout 上游重置时间的格式。
const resetTimeLayout = "2006-01-02 15:04:05"

// resetLoc 重置文案的固定时区：按 UTC+8 解释（上游文案如此，与容器时区无关）。
var resetLoc = time.FixedZone("UTC+8", 8*60*60)

// ParseRateReset 从任何限流响应 body 里统一解析「将在 … 重置」时间（UTC+8 文案）。
// 成功返回解析出的**墙钟时刻**，失败返回零值 + false。
// 没有时间文案的限流也照常由调用方退回有界退避（绝不臆造时间）。
func ParseRateReset(body string) (time.Time, bool) {
	m := reResetCN.FindStringSubmatch(body)
	if len(m) < 2 {
		m = reResetEN.FindStringSubmatch(body)
	}
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8")
	t, err := time.ParseInLocation(resetTimeLayout, ts, resetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// containsAny 报告 body 是否命中词表（小写与原文双通道）。
func containsAny(body, lower string, markers []string) bool {
	for _, m := range markers {
		lm := strings.ToLower(m)
		if strings.Contains(lower, lm) || strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// isPromptTooLongStatus 上下文超限只在请求级 4xx 上判（429+超限属限流语义优先，
// 5xx 属服务端故障优先）。
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
//
// 判定顺序自「严」到「宽」：
//  1. 402 —— 真正的配额耗尽状态码，最严、最不可自愈，最先判。
//  2. sessionDead / 401/403 —— 需要人工修 token 的终态，先于限流判定
//     （401 body 混排 "rate limit" 不得误归限流：具体优先于宽泛）。
//  3. status==429 —— 限流状态码兜底（先于 hardQuotaMarkers）：429 body 高频
//     携带 "quota exceeded" 等跨计费/限流两界的措辞，关键词先判会把限流误归
//     硬冷却弃号半天。状态码是比关键词更权威的信号；真正的配额耗尽由 402
//     或非 429 的 quota 文案捕获。
//  4. hardQuotaMarkers —— 非 429 响应携带配额关键词。
//  5. softRateMarkers —— 非 429 状态码携带限流文案（可覆盖 200/400/403/5xx）。
//  6. promptTooLong —— 判在 404/5xx 与通用 4xx 兜底之前（404 上若归 ErrNotFound
//     会误冷却账号——上下文超限与账号无关）。
//  7. 404 / 5xx —— 与限流无关的常规分类。
//  8. 内容策略 / 其他 4xx —— 通用兜底。
func Classify(status int, body string) ErrKind {
	// 402：配额耗尽状态码，最先判。
	if status == http.StatusPaymentRequired {
		return ErrHardQuota
	}
	lower := strings.ToLower(body)
	// 认证失效先于 status==429：终态等不来自愈，限流状态码不得掩盖。
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return ErrAuthDead
	}
	if containsAny(body, lower, sessionDeadMarkers) {
		return ErrAuthDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if containsAny(body, lower, hardQuotaMarkers) {
		return ErrHardQuota
	}
	if containsAny(body, lower, softRateMarkers) {
		return ErrSoftRate
	}
	// 上下文超限：判在 404/5xx/通用 4xx 之前——请求级语义最具体。
	if isPromptTooLongStatus(status) && containsAny(body, lower, promptTooLongMarkers) {
		return ErrPromptTooLong
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		if containsAny(body, lower, contentBlockedMarkers) {
			return ErrContentBlocked
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含配额/限流关键词的情况已被上面各层捕获。
	return ErrNone
}
