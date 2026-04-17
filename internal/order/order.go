package order

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kykira/ws-order-go/internal/config"
	"github.com/kykira/ws-order-go/internal/logs"
)

type Client struct {
	logger      *logs.Logger
	clientsMu   sync.RWMutex
	httpClients map[string]*http.Client
}

func NewClient(logger *logs.Logger) *Client {
	return &Client{
		logger:      logger,
		httpClients: make(map[string]*http.Client),
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
	// Optionally we could call CloseIdleConnections on each transport before throwing them away
	for _, client := range c.httpClients {
		if tr, ok := client.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	c.httpClients = make(map[string]*http.Client)
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

	// Parse Headers
	lines := strings.Split(task.Headers, "\n")
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
		}
	}

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

func (c *Client) httpClientForTask(task config.TaskConfig) (*http.Client, error) {
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

	// Build a per-task client so each task can use its own proxy.
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if strings.TrimSpace(task.HTTPProxyURL) != "" {
		u, err := url.Parse(strings.TrimSpace(task.HTTPProxyURL))
		if err != nil {
			return nil, fmt.Errorf("invalid httpProxyUrl: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}

	newClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: tr,
	}

	c.httpClients[task.ID] = newClient
	return newClient, nil
}
