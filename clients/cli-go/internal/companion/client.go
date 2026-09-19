package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	wire "github.com/edu-agent/edu-agent/packages/agentcore/companion"
)

type Client struct {
	Origin   string
	HTTP     *http.Client
	sequence uint64
}

func NewClient(origin string, allowHTTP bool) (*Client, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("服务地址必须是 HTTPS 根地址")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("仅显式开发允许回环 IP 的 HTTP")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{Origin: strings.TrimSuffix(origin, "/"), HTTP: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) post(ctx context.Context, path string, value any, id, token string, result any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", c.Origin+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", c.Origin)
	if token != "" {
		c.sequence++
		r.Header.Set("X-Companion-ID", id)
		r.Header.Set("X-Companion-Sequence", strconv.FormatUint(c.sequence, 10))
		r.Header.Set("X-Companion-MAC", wire.MAC(token, c.Origin, id, c.sequence, body))
	}
	response, err := c.HTTP.Do(r)
	if err != nil {
		return errors.New("companion_network_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 404 {
		return wire.ErrDenied
	}
	if response.StatusCode != 200 {
		return errors.New("companion_channel_failed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, wire.MaxPayload+1))
	if err != nil || len(data) > wire.MaxPayload {
		return errors.New("companion_invalid_response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(result); err != nil {
		return errors.New("companion_invalid_response")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("companion_invalid_response")
	}
	return nil
}

func (c *Client) Attach(ctx context.Context, a wire.Attach) (wire.Channel, error) {
	var channel wire.Channel
	err := c.post(ctx, "/v1/companion/attach", a, "", "", &channel)
	return channel, err
}

// Run 只重试通道观察和已完成回执；绝不重试操作。超时即本地收尾。
func (c *Client) Run(ctx context.Context, id, token string, p *Provider) error {
	last := time.Now()
	var receipt *wire.Receipt
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(last) > wire.Lease {
			return errors.New("companion_lease_expired")
		}
		var d wire.Delivery
		err := c.post(ctx, "/v1/companion/channel", wire.Exchange{Receipt: receipt}, id, token, &d)
		if errors.Is(err, wire.ErrDenied) {
			return err
		}
		if err == nil {
			last = time.Now()
			receipt = nil
			if d.Operation != nil {
				if d.Grant == nil {
					return wire.ErrDenied
				}
				opctx, cancel := context.WithTimeout(ctx, 32*time.Second)
				r := p.Execute(opctx, *d.Grant, *d.Operation)
				cancel()
				receipt = &r
				continue
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
