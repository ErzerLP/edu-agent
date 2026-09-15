// Package websearch 定义与供应商无关的搜索边界；结果不是网页抓取授权。
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type Request struct {
	Query string
	Limit int
}
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}
type Adapter interface {
	Search(context.Context, Request) ([]Result, error)
}
type Error struct{ Reason string }

func (e *Error) Error() string { return "搜索请求失败: " + e.Reason }
func Category(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	var n net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &n) && n.Timeout()) {
		return "timeout"
	}
	return "unavailable"
}

type Brave struct {
	endpoint string
	key      string
	client   *http.Client
}

func NewBrave(endpoint, key string, client *http.Client) *Brave { return &Brave{endpoint, key, client} }
func (b *Brave) Search(ctx context.Context, input Request) ([]Result, error) {
	if len(input.Query) == 0 || len(input.Query) > 1024 || input.Limit < 1 || input.Limit > 20 {
		return nil, &Error{"invalid_request"}
	}
	u, err := url.Parse(b.endpoint)
	if err != nil {
		return nil, &Error{"invalid_request"}
	}
	q := u.Query()
	q.Set("q", input.Query)
	q.Set("count", strconv.Itoa(input.Limit))
	u.RawQuery = q.Encode()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Error{"invalid_request"}
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("X-Subscription-Token", b.key)
	response, err := b.client.Do(r)
	if err != nil {
		return nil, &Error{Category(err)}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		reason := "upstream_error"
		switch response.StatusCode {
		case 401, 403:
			reason = "unauthorized"
		case 429:
			reason = "rate_limited"
		case 301, 302, 303, 307, 308:
			reason = "redirect_rejected"
		}
		return nil, &Error{reason}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(body) > 256<<10 {
		return nil, &Error{"invalid_response"}
	}
	var data struct {
		Web *struct {
			Results []Result `json:"results"`
		} `json:"web"`
	}
	if json.Unmarshal(body, &data) != nil || data.Web == nil || data.Web.Results == nil {
		return nil, &Error{"invalid_response"}
	}
	if len(data.Web.Results) > input.Limit {
		data.Web.Results = data.Web.Results[:input.Limit]
	}
	for _, result := range data.Web.Results {
		u, err := url.Parse(result.URL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || len(result.Title) > 4096 || len(result.URL) > 8192 || len(result.Description) > 16384 || strings.TrimSpace(result.Title) == "" {
			return nil, &Error{"invalid_response"}
		}
	}
	return data.Web.Results, nil
}
