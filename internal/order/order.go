package order

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

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
	IsTest     bool
}

// ClearCache removes all cached HTTP clients, forcing them to be recreated.
// Call this when configuration changes.
func (c *Client) ClearCache() {
	c.clientsMu.Lock()
	defer c.clientsMu.Unlock()
	c.httpClients = make(map[string]tls_client.HttpClient)
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
	bodyStr := task.Body
	bodyStr = strings.ReplaceAll(bodyStr, "{{amount}}", req.Amount)
	bodyStr = strings.ReplaceAll(bodyStr, "{{unit}}", req.Unit)
	bodyStr = strings.ReplaceAll(bodyStr, "{{action}}", actVal)
	bodyStr = strings.ReplaceAll(bodyStr, "{{symbol}}", req.Symbol)
	bodyStr = strings.ReplaceAll(bodyStr, "{{tickerType}}", req.TickerType)
	// Alias for backward compatibility
	bodyStr = strings.ReplaceAll(bodyStr, "{{direction}}", actVal)

	urlStr := task.APIUrl
	urlStr = strings.ReplaceAll(urlStr, "{{amount}}", req.Amount)
	urlStr = strings.ReplaceAll(urlStr, "{{unit}}", req.Unit)
	urlStr = strings.ReplaceAll(urlStr, "{{action}}", actVal)
	urlStr = strings.ReplaceAll(urlStr, "{{symbol}}", req.Symbol)
	urlStr = strings.ReplaceAll(urlStr, "{{tickerType}}", req.TickerType)
	// Alias for backward compatibility
	urlStr = strings.ReplaceAll(urlStr, "{{direction}}", actVal)

	method := strings.ToUpper(strings.TrimSpace(task.Method))
	if method == "" {
		method = "POST"
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, urlStr, strings.NewReader(bodyStr))
	if err != nil {
		c.logger.Error("order", fmt.Sprintf("create request error: %v", err))
		return err
	}

	httpReq.Header = http.Header{
		"User-Agent":      {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"},
		"accept":          {"*/*"},
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
	lines := strings.Split(task.Headers, "\n")
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

	c.logger.Info("order", fmt.Sprintf("%stask=[%s] START %s %s\nBody: %s\nProxy: %s",
		tag, task.Name, method, urlStr, bodyStr, task.HTTPProxyURL))

	httpClient, err := c.httpClientForTask(task)
	if err != nil {
		c.logger.Error("order", fmt.Sprintf("%stask=[%s] http client error: %v", tag, task.Name, err))
		return err
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		c.logger.Error("order", fmt.Sprintf("%stask=[%s] http error: %v", tag, task.Name, err))
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	c.logger.Info("order", fmt.Sprintf("%stask=[%s] FINISH status=%d\nResponse: %s", tag, task.Name, resp.StatusCode, string(respBody)))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
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
		tls_client.WithCookieJar(jar),
	}

	if strings.TrimSpace(task.HTTPProxyURL) != "" {
		options = append(options, tls_client.WithProxyUrl(strings.TrimSpace(task.HTTPProxyURL)))
	}

	newClient, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		log.Println(err)
		return nil, fmt.Errorf("create new http client error: %v", err)
	}

	c.httpClients[task.ID] = newClient
	return newClient, nil
}
