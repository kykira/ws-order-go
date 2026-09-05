package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

// ErrRetryExhausted indicates the exchange rejected the order with a retryable
// error (e.g. Binance 93420018 open order limit, TurboFlow 1021110 order too
// frequent) and all local retries were exhausted.
var ErrRetryExhausted = errors.New("retryable order error exhausted after retries")

// ErrAccountUnavailable indicates the account itself is no longer usable,
// e.g. login status expired (100002001) or banned (93420004).
var ErrAccountUnavailable = errors.New("account unavailable or expired")

type Client struct {
	logger      *logs.Logger
	clientsMu   sync.RWMutex
	httpClients map[string]tls_client.HttpClient
}

func NewClient(logger *logs.Logger) *Client {
	return &Client{
		logger:      logger,
		httpClients: make(map[string]tls_client.HttpClient),
	}
}

type PlaceOrderRequest struct {
	Amount     string
	Unit       string
	Action     string
	Symbol     string
	TickerType string
	Period     string
	IsTest     bool
}

func periodToSeconds(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "30s":
		return "30"
	case "45s":
		return "45"
	case "1m":
		return "60"
	case "3m":
		return "180"
	case "5m":
		return "300"
	case "15m":
		return "900"
	case "30m":
		return "1800"
	case "1h":
		return "3600"
	case "2h":
		return "7200"
	case "4h":
		return "14400"
	default:
		return strings.TrimSpace(period)
	}
}

func periodToMinutes(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "1m":
		return "1"
	case "3m":
		return "3"
	case "5m":
		return "5"
	case "15m":
		return "15"
	case "30m":
		return "30"
	case "1h":
		return "60"
	case "2h":
		return "120"
	case "4h":
		return "240"
	default:
		return strings.TrimSpace(period)
	}
}

// bizResponse covers both Binance-style responses and common third-party
// success formats such as {"errno":"200","msg":"success"}.
type bizResponse struct {
	Code    string `json:"code"`
	Success bool   `json:"success"`
	Errno   string `json:"errno"`
	Msg     string `json:"msg"`
}

func (b bizResponse) isSuccess() bool {
	if b.Code == "000000" || b.Success {
		return true
	}
	if b.Errno == "200" || strings.EqualFold(strings.TrimSpace(b.Msg), "success") {
		return true
	}
	return false
}

// ClearCache removes all cached HTTP clients, forcing them to be recreated.
// Call this when configuration changes.
func (c *Client) ClearCache() {
	c.clientsMu.Lock()
	defer c.clientsMu.Unlock()
	c.httpClients = make(map[string]tls_client.HttpClient)
}

func isAccountUnavailableCode(code string) bool {
	return code == "100002001" || code == "93420004"
}

func binanceTimeIncrement(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "5m":
		return "FIVE_MINUTE"
	case "15m":
		return "FIFTEEN_MINUTE"
	case "30m":
		return "THIRTY_MINUTE"
	case "1h":
		return "ONE_HOUR"
	case "2h":
		return "TWO_HOUR"
	case "4h":
		return "FOUR_HOUR"
	default:
		return "THIRTY_MINUTE"
	}
}

func hibtSymbol(symbol string) string {
	s := strings.ToLower(strings.TrimSpace(symbol))
	if strings.HasSuffix(s, "usdt") {
		return strings.TrimSuffix(s, "usdt") + "_usdt"
	}
	return s
}

func defaultURLForType(accountType string) string {
	switch accountType {
	case "hibt":
		return "https://api.hibt0.com/option/option-order/place?v={{v}}"
	case "turboflow":
		return "https://apis.turboflow.xyz/account/pm/order/submit"
	case "binance":
		return "https://www.binance.com/bapi/futures/v2/private/future/event-contract/place-order"
	default:
		return ""
	}
}

func defaultBodyForType(accountType string) string {
	switch accountType {
	case "hibt":
		return "amount={{amount}}&direction={{action}}&symbol={{hibt_symbol}}&timeUnit={{timeUnit}}&langCode=zh_CN"
	case "turboflow":
		return `{"account_id":"{{account_id}}","amount":"{{amount}}","coin_code":"{{coin_code}}","duration":{{duration}},"order_way":{{action}},"pair_id":"{{pair_id}}","pool_id":{{pool_id}},"return_rate":{{return_rate}}}`
	case "binance":
		return `{"direction":"{{action}}","timeIncrements":"{{binance_time}}","orderAmount":{{amount}},"payoutRatio":0.85,"walletType":"UM","symbolName":"{{symbol}}"}`
	default:
		return ""
	}
}

func defaultHeadersForType(task config.TaskConfig) string {
	switch task.Type {
	case "turboflow":
		return strings.TrimSpace(fmt.Sprintf("accept: application/json, text/plain, */*\nauthorization: %s\nUser-Agent: Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36\nbiz-pf: %s\ncontent-type: application/json\nlang: zh-cn\nuid: %s\norigin: https://www.turboflow.xyz\nreferer: https://www.turboflow.xyz/",
			task.Auth["authorization"], task.Auth["biz-pf"], task.Auth["uid"]))
	case "hibt":
		return strings.TrimSpace(fmt.Sprintf("accept: application/json, text/plain, */*\nclient-type: web\ncontent-type: application/x-www-form-urlencoded\nhc-language: zh_CN\nhc-platform: web\nlang: zh_CN\norigin: https://hibt.com\nplatform: PC\nreferer: https://hibt.com/\nuser-agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36\nauthorization: %s\nx-auth-token: %s",
			task.Auth["authorization"], task.Auth["x-auth-token"]))
	case "binance":
		return strings.TrimSpace(fmt.Sprintf("content-type: application/json\nclienttype: web\ncsrftoken: %s\nCookie: p20t=%s",
			task.Auth["csrftoken"], task.Auth["p20t"]))
	default:
		return ""
	}
}

func (c *Client) PlaceOrder(ctx context.Context, task config.TaskConfig, req PlaceOrderRequest) error {
	// Resolve dynamic action value
	actVal := req.Action
	if req.Action == "buy" && task.ValueBuy != "" {
		actVal = task.ValueBuy
	} else if req.Action == "sell" && task.ValueSell != "" {
		actVal = task.ValueSell
	}

	// Template replacement
	accountID := task.Auth["account_id"]
	if accountID == "" {
		accountID = task.Auth["uid"]
	}
	authToken := task.Auth["authorization"]
	if authToken == "" {
		authToken = task.Auth["x-auth-token"]
	}
	v := task.Auth["v"]
	hibtSym := hibtSymbol(req.Symbol)

	bodyStr := defaultBodyForType(task.Type)
	urlStr := defaultURLForType(task.Type)
	headersStr := defaultHeadersForType(task)
	if task.Type == "raw" {
		bodyStr = task.Body
		urlStr = task.APIUrl
		headersStr = task.Headers
	}

	bizPf := task.Auth["biz-pf"]
	symParams := task.Symbols[req.Symbol]
	coinCode := "1"
	pairID := bizPf
	poolID := "1"
	returnRate := "85"
	duration := periodToSeconds(req.Period)
	if symParams != nil {
		if symParams["coin_code"] != "" {
			coinCode = symParams["coin_code"]
		}
		if symParams["pair_id"] != "" {
			pairID = symParams["pair_id"]
		}
		if symParams["pool_id"] != "" {
			poolID = symParams["pool_id"]
		}
		if symParams["return_rate"] != "" {
			returnRate = symParams["return_rate"]
		}
	}
	binanceTime := binanceTimeIncrement(req.Period)

	bodyStr = strings.ReplaceAll(bodyStr, "{{amount}}", req.Amount)
	bodyStr = strings.ReplaceAll(bodyStr, "{{unit}}", req.Unit)
	bodyStr = strings.ReplaceAll(bodyStr, "{{action}}", actVal)
	bodyStr = strings.ReplaceAll(bodyStr, "{{symbol}}", req.Symbol)
	bodyStr = strings.ReplaceAll(bodyStr, "{{hibt_symbol}}", hibtSym)
	bodyStr = strings.ReplaceAll(bodyStr, "{{v}}", v)
	bodyStr = strings.ReplaceAll(bodyStr, "{{tickerType}}", req.TickerType)
	// Alias for backward compatibility
	bodyStr = strings.ReplaceAll(bodyStr, "{{direction}}", actVal)
	// Strategy/platform period fields
	bodyStr = strings.ReplaceAll(bodyStr, "{{period}}", strings.TrimSpace(req.Period))
	bodyStr = strings.ReplaceAll(bodyStr, "{{duration}}", duration)
	bodyStr = strings.ReplaceAll(bodyStr, "{{timeUnit}}", periodToMinutes(req.Period))
	bodyStr = strings.ReplaceAll(bodyStr, "{{binance_time}}", binanceTime)
	bodyStr = strings.ReplaceAll(bodyStr, "{{account_id}}", accountID)
	bodyStr = strings.ReplaceAll(bodyStr, "{{uid}}", accountID)
	bodyStr = strings.ReplaceAll(bodyStr, "{{biz_pf}}", bizPf)
	bodyStr = strings.ReplaceAll(bodyStr, "{{coin_code}}", coinCode)
	bodyStr = strings.ReplaceAll(bodyStr, "{{pair_id}}", pairID)
	bodyStr = strings.ReplaceAll(bodyStr, "{{pool_id}}", poolID)
	bodyStr = strings.ReplaceAll(bodyStr, "{{return_rate}}", returnRate)
	bodyStr = strings.ReplaceAll(bodyStr, "{{token}}", authToken)

	urlStr = strings.ReplaceAll(urlStr, "{{amount}}", req.Amount)
	urlStr = strings.ReplaceAll(urlStr, "{{unit}}", req.Unit)
	urlStr = strings.ReplaceAll(urlStr, "{{action}}", actVal)
	urlStr = strings.ReplaceAll(urlStr, "{{symbol}}", req.Symbol)
	urlStr = strings.ReplaceAll(urlStr, "{{hibt_symbol}}", hibtSym)
	urlStr = strings.ReplaceAll(urlStr, "{{v}}", v)
	urlStr = strings.ReplaceAll(urlStr, "{{tickerType}}", req.TickerType)
	// Alias for backward compatibility
	urlStr = strings.ReplaceAll(urlStr, "{{direction}}", actVal)
	urlStr = strings.ReplaceAll(urlStr, "{{period}}", strings.TrimSpace(req.Period))
	urlStr = strings.ReplaceAll(urlStr, "{{duration}}", duration)
	urlStr = strings.ReplaceAll(urlStr, "{{timeUnit}}", periodToMinutes(req.Period))
	urlStr = strings.ReplaceAll(urlStr, "{{binance_time}}", binanceTime)
	urlStr = strings.ReplaceAll(urlStr, "{{account_id}}", accountID)
	urlStr = strings.ReplaceAll(urlStr, "{{uid}}", accountID)
	urlStr = strings.ReplaceAll(urlStr, "{{biz_pf}}", bizPf)
	urlStr = strings.ReplaceAll(urlStr, "{{coin_code}}", coinCode)
	urlStr = strings.ReplaceAll(urlStr, "{{pair_id}}", pairID)
	urlStr = strings.ReplaceAll(urlStr, "{{pool_id}}", poolID)
	urlStr = strings.ReplaceAll(urlStr, "{{return_rate}}", returnRate)
	urlStr = strings.ReplaceAll(urlStr, "{{token}}", authToken)

	method := "POST"
	if task.Type == "raw" && strings.TrimSpace(task.Method) != "" {
		method = strings.ToUpper(strings.TrimSpace(task.Method))
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, urlStr, strings.NewReader(bodyStr))
	if err != nil {
		c.logger.Error("order", fmt.Sprintf("create request error: %v", err))
		return err
	}
	// Allow body re-reading for retries
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(bodyStr)), nil
	}

	httpReq.Header = http.Header{
		"User-Agent":      {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"},
		"accept":          {"*/*"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
		"Accept-Encoding": {"gzip, deflate, br, zstd"},
		"Cache-Control":   {"no-cache"},
		"Origin":          {"https://www.binance.com"},
		"Referer":         {"https://www.binance.com/"},
		"Sec-Fetch-Site":  {"cross-site"},
		"Sec-Fetch-Mode":  {"cors"},
		"Sec-Fetch-Dest":  {"empty"},
		http.HeaderOrderKey: {
			"User-Agent",
			"accept",
			"Accept-Encoding",
			"Cache-Control",
			"Origin",
			"Referer",
			"Sec-Fetch-Site",
			"Sec-Fetch-Mode",
			"Sec-Fetch-Dest",
		},
	}
	// Parse Headers
	lines := strings.Split(headersStr, "\n")
	headerOrder := httpReq.Header[http.HeaderOrderKey]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" {
			httpReq.Header.Set(k, v)
			headerOrder = append(headerOrder, k)
		}
	}
	httpReq.Header[http.HeaderOrderKey] = headerOrder

	tag := ""
	if req.IsTest {
		tag = "[TEST] "
	}

	c.logger.Info("order", fmt.Sprintf("%stask=[%s] START %s %s\nBody: %s",
		tag, task.Name, method, urlStr, bodyStr))

	httpClient, err := c.httpClientForTask(task)
	if err != nil {
		c.logger.Error("order", fmt.Sprintf("%stask=[%s] http client error: %v", tag, task.Name, err))
		return err
	}

	// Retry once on "Open order number has reached maximum limit" (93420018)
	maxRetries := 2
	sawRetryable := false
	var lastRespBody []byte
	var lastStatusCode int
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s, 8s
			c.logger.Info("order", fmt.Sprintf("%stask=[%s] retry %d/%d after %v", tag, task.Name, attempt, maxRetries-1, delay))
			select {
			case <-ctx.Done():
				if sawRetryable {
					return fmt.Errorf("%w: context deadline exceeded after retryable error retries", ErrRetryExhausted)
				}
				return ctx.Err()
			case <-time.After(delay):
			}
			// Reset request body for retry
			httpReq.Body = io.NopCloser(strings.NewReader(bodyStr))
		}

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			if sawRetryable && errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%w: http request deadline exceeded after retryable error retries", ErrRetryExhausted)
			}
			c.logger.Error("order", fmt.Sprintf("%stask=[%s] http error (attempt %d): %v", tag, task.Name, attempt+1, err))
			return err
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastRespBody = respBody
		lastStatusCode = resp.StatusCode

		c.logger.Info("order", fmt.Sprintf("%stask=[%s] FINISH status=%d\nResponse: %s", tag, task.Name, resp.StatusCode, string(respBody)))

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var bizResp bizResponse
			if json.Unmarshal(respBody, &bizResp) == nil {
				if bizResp.Code == "93420018" || bizResp.Errno == "1021110" {
					sawRetryable = true
					c.logger.Info("order", fmt.Sprintf("%stask=[%s] retryable error code=%s errno=%s, will retry", tag, task.Name, bizResp.Code, bizResp.Errno))
					continue
				}
				if bizResp.isSuccess() {
					return nil
				}
				if isAccountUnavailableCode(bizResp.Code) {
					c.logger.Error("order", fmt.Sprintf("%stask=[%s] business error code=%s (account unavailable/expired)", tag, task.Name, bizResp.Code))
					return fmt.Errorf("%w: binance error code=%s", ErrAccountUnavailable, bizResp.Code)
				}
				// Other business error — don't retry
				c.logger.Error("order", fmt.Sprintf("%stask=[%s] business error code=%s", tag, task.Name, bizResp.Code))
				return fmt.Errorf("binance error code=%s", bizResp.Code)
			}
			return nil
		}

		var bizResp bizResponse
		if json.Unmarshal(respBody, &bizResp) == nil && isAccountUnavailableCode(bizResp.Code) {
			c.logger.Error("order", fmt.Sprintf("%stask=[%s] account unavailable/expired status=%d code=%s", tag, task.Name, resp.StatusCode, bizResp.Code))
			return fmt.Errorf("%w: status=%d binance error code=%s", ErrAccountUnavailable, resp.StatusCode, bizResp.Code)
		}

		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	c.logger.Error("order", fmt.Sprintf("%stask=[%s] max retries exhausted, last status=%d body=%s",
		tag, task.Name, lastStatusCode, string(lastRespBody)))
	return fmt.Errorf("%w: max retries (%d) exhausted on retryable error", ErrRetryExhausted, maxRetries)
}

func (c *Client) httpClientForTask(task config.TaskConfig) (tls_client.HttpClient, error) {
	c.clientsMu.RLock()
	client, exists := c.httpClients[task.ID]
	c.clientsMu.RUnlock()
	if exists {
		return client, nil
	}

	c.clientsMu.Lock()
	defer c.clientsMu.Unlock()
	// Double check
	if client, exists := c.httpClients[task.ID]; exists {
		return client, nil
	}

	// 直接创建客户端
	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_144),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithInsecureSkipVerify(),
		tls_client.WithCookieJar(jar),
	}

	newClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		log.Println(err)
		return nil, fmt.Errorf("create new http client error: %v", err)
	}

	c.httpClients[task.ID] = newClient
	return newClient, nil
}
