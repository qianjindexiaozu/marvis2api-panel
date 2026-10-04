package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/logfmt"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/ual"
)

const (
	defaultWalletURL  = "https://yybadaccess.3g.qq.com/v3/marvis_get_wallet"
	marvisBusinessID  = "marvis_client"
	marvisLoginTypePC = "6"
)

// ErrNoGUID 账号没带 guid，每日额度接口不会返回数据。
var ErrNoGUID = fmt.Errorf("这个账号没有 guid，无法查询每日额度")

// walletQuota 查询 Marvis 每日额度。
// 总额是 total_tokens（每天 1000 万），剩余是 avail_tokens。
// quota_path 留空时走这里；填了 quota_path 仍用原来的通用查询。
func (c *Client) walletQuota(ctx context.Context, a *account.Auth) (QuotaResponse, error) {
	if a == nil {
		return QuotaResponse{}, fmt.Errorf("account is nil")
	}
	guid := strings.TrimSpace(a.Guid)
	if guid == "" {
		return QuotaResponse{}, ErrNoGUID
	}
	token := strings.TrimSpace(a.TokenValue())
	if token == "" {
		return QuotaResponse{}, fmt.Errorf("token 不能为空")
	}
	openid := strings.TrimSpace(a.UID)
	body := []byte("{}")
	ts, nonce, reqID := walletNonce()
	sig, err := ual.Sign(body, ts, nonce, c.AccessKey)
	if err != nil {
		return QuotaResponse{}, err
	}
	endpoint := c.WalletURL
	if endpoint == "" {
		endpoint = defaultWalletURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return QuotaResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ual-Access-Businessid", marvisBusinessID)
	req.Header.Set("Ual-Access-Timestamp", ts)
	req.Header.Set("Ual-Access-Nonce", nonce)
	req.Header.Set("Ual-Access-Signature", sig)
	req.Header.Set("Ual-Access-Requestid", reqID)
	req.Header.Set("Ual-Access-Access-Token", token)
	req.Header.Set("Ual-Access-Login-Type", marvisLoginTypePC)
	req.Header.Set("Ual-Access-Openid", openid)
	req.Header.Set("Ual-Access-Guid", guid)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return QuotaResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QuotaResponse{}, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return QuotaResponse{}, &Error{Kind: kind, Status: resp.StatusCode, Msg: logfmt.Truncate(string(raw), 200)}
	}
	return parseWallet(raw)
}

func walletNonce() (ts, nonce, reqID string) {
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		n = big.NewInt(1)
	}
	r, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		r = big.NewInt(1)
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 10), n.String(), r.String()
}

func parseWallet(raw []byte) (QuotaResponse, error) {
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return QuotaResponse{}, fmt.Errorf("quota 响应无法解析")
	}
	if env.Code != 0 {
		msg := strings.TrimSpace(env.Msg)
		if msg == "" {
			msg = "钱包接口返回失败"
		}
		return QuotaResponse{}, fmt.Errorf("%s", logfmt.Truncate(msg, 120))
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return QuotaResponse{}, fmt.Errorf("钱包没有返回额度")
	}
	var data struct {
		Total json.RawMessage `json:"total_tokens"`
		Avail json.RawMessage `json:"avail_tokens"`
		Used  json.RawMessage `json:"used_tokens"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return QuotaResponse{}, fmt.Errorf("quota 响应无法解析")
	}
	total, okT := jsonInt(data.Total)
	avail, okA := jsonInt(data.Avail)
	used, okU := jsonInt(data.Used)
	if !okA && okT && okU && total >= used {
		avail = total - used
		okA = true
	}
	if !okA && !okT {
		return QuotaResponse{}, fmt.Errorf("钱包响应里没有额度数字")
	}
	return QuotaResponse{Remaining: avail, Total: total}, nil
}

func jsonInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
