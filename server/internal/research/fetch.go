package research

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/pdfsource"

	"github.com/google/uuid"
)

// Enough for real documentation pages (go.dev/dl is over 3 MB), while
// retaining independent bounds on downloads and decompression.
const MaxWire = 4 << 20
const MaxDecoded = 8 << 20

var denied = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func PublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range denied {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || len(raw) > 8192 || strings.ContainsAny(raw, "\\\x00\r\n\t ") || u.Scheme != "https" && u.Scheme != "http" {
		return nil, ErrPolicy
	}
	if u.Port() != "" && (u.Scheme == "https" && u.Port() != "443" || u.Scheme == "http" && u.Port() != "80") {
		return nil, ErrPolicy
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") || strings.HasSuffix(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") || !strings.ContainsAny(host, ".:") {
		return nil, ErrPolicy
	}
	if ip, e := netip.ParseAddr(host); e == nil && !PublicIP(ip) {
		return nil, ErrPolicy
	}
	u.Fragment = ""
	return u, nil
}

type Fetcher struct {
	lookup func(context.Context, string, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

func NewFetcher() *Fetcher {
	return &Fetcher{lookup: net.DefaultResolver.LookupNetIP, dial: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
}

// NewFetcherWithNetwork 供受信宿主注入网络依赖；地址校验和固定拨号仍由抓取器执行。
func NewFetcherWithNetwork(lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) *Fetcher {
	return &Fetcher{lookup: lookup, dial: dial}
}

func (f *Fetcher) transport() *http.Transport {
	return &http.Transport{DisableCompression: true, DisableKeepAlives: true, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32 << 10,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, ErrPolicy
			}
			ips, err := f.lookup(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, errors.Join(errDNSFailed, err)
			}
			for _, ip := range ips {
				if !PublicIP(ip) {
					return nil, ErrPolicy
				}
			}
			// 只拨号已检查的数字地址，连接阶段不再次解析域名。
			return f.dial(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
	}
}

var errDNSFailed = errors.New("dns_failed")

func failure(err error) string {
	if errors.Is(err, ErrPolicy) {
		return "network_policy_rejected"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var n net.Error
	var dns *net.DNSError
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &dns) && dns.Timeout() || errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	if errors.Is(err, errDNSFailed) {
		return "dns_failed"
	}
	return "network_failed"
}

// 每次 GET（包括跳转）先经过预算与租约回调；不携带 Cookie、认证或代理配置。
func (f *Fetcher) Fetch(ctx context.Context, source Source, policy Policy, before func() error) (Source, error) {
	// A discovery candidate's title is not the fetched page's title.
	source.Title = ""
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	transport := f.transport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	raw := source.Locator
	for hop := 0; hop <= 4; hop++ {
		u, err := ValidateURL(raw)
		if err != nil || !policy.Allows(raw) {
			source.Status = "failed"
			source.Failure = "network_policy_rejected"
			return source, nil
		}
		if err = before(); err != nil {
			return source, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		req.Header.Set("User-Agent", "edu-agent-research/1.0")
		req.Header.Set("Accept", "text/html,text/plain,text/markdown,application/pdf")
		resp, err := client.Do(req)
		if err != nil {
			source.Status = "failed"
			source.Failure = failure(err)
			return source, nil
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location, err := resp.Location()
			resp.Body.Close()
			if err != nil || hop == 4 {
				source.Status = "failed"
				source.Failure = "redirect_limit"
				return source, nil
			}
			raw = location.String()
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			source.Status = "failed"
			source.Failure = "http_" + strconv.Itoa(resp.StatusCode)
			return source, nil
		}
		wire, err := io.ReadAll(io.LimitReader(resp.Body, MaxWire+1))
		resp.Body.Close()
		if err != nil {
			source.Status, source.Failure = "failed", failure(err)
			if source.Failure == "network_failed" {
				source.Failure = "body_read_failed"
			}
			return source, nil
		}
		if len(wire) > MaxWire {
			source.Status = "failed"
			source.Failure = "body_limit"
			return source, nil
		}
		body := wire
		switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
		case "gzip":
			reader, e := gzip.NewReader(bytes.NewReader(wire))
			if e != nil {
				source.Status = "failed"
				source.Failure = "invalid_compression"
				return source, nil
			}
			body, err = io.ReadAll(io.LimitReader(reader, MaxDecoded+1))
			reader.Close()
		case "", "identity":
		default:
			source.Status = "failed"
			source.Failure = "unsupported_encoding"
			return source, nil
		}
		if err != nil {
			source.Status, source.Failure = "failed", "invalid_compression"
			return source, nil
		}
		if len(body) > MaxDecoded {
			source.Status = "failed"
			source.Failure = "decoded_limit"
			return source, nil
		}
		media, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		now := time.Now().UTC()
		source.FetchedAt = &now
		source.FinalURL = u.String()
		source.Kind = media
		if err != nil || media != "text/html" && media != "text/plain" && media != "text/markdown" && media != "application/pdf" {
			source.Status = "failed"
			source.Failure = "unsupported_format"
			return source, nil
		}

		// 声明不保存或不归档的页面不进入缓存、模型或可采纳候选。
		restrictions := strings.ToLower(strings.Join(resp.Header.Values("Cache-Control"), ",") + "," + strings.Join(resp.Header.Values("X-Robots-Tag"), ","))
		if restricted(restrictions) {
			source.Status = "failed"
			source.Failure = "storage_restricted"
			return source, nil
		}
		if media == "application/pdf" {
			report, err := pdfsource.Parse(ctx, body)
			if err != nil {
				source.Status, source.Failure = "failed", pdfsource.Code(err)
				return source, nil
			}
			if strings.TrimSpace(report.Text()) == "" {
				source.Status, source.Failure = "failed", "pdf_no_text"
				return source, nil
			}
			source.PDF, source.PDFOriginal = &report, body
			source.Text, source.Coverage, source.Parser, source.Fingerprint = report.Text(), report.Coverage, report.Parser, report.Fingerprint
			source.RevisionID = uuid.NewString()
			source.Status, source.StorageAllowed = "parsed", true
			if report.Coverage != "complete_text" {
				source.Status = "partial"
			}
			for _, page := range report.Pages {
				if page.Text == "" {
					continue
				}
				for _, f := range fragments(page.Text) {
					f.Start += page.Start
					f.End += page.Start
					f.Page = page.Number
					source.Fragments = append(source.Fragments, f)
				}
			}
			return source, nil
		}
		textBody, decodeFailure := decodeText(body, media, params["charset"])
		if decodeFailure != "" {
			source.Status, source.Failure = "failed", decodeFailure
			return source, nil
		}
		text, coverage := string(textBody), "complete_text"
		title := ""
		if media == "text/html" {
			text, coverage, title = parseHTML(textBody)
			source.Links = publicLinks(textBody, u)
		}
		if coverage == "storage_restricted" {
			source.Status = "failed"
			source.Failure = coverage
			return source, nil
		}
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
		if len(text) > MaxText {
			text = text[:MaxText]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			coverage = "partial_text_limit"
		}
		if strings.TrimSpace(text) == "" {
			source.Status = "failed"
			source.Failure = "parse_empty"
			return source, nil
		}
		sum := sha256.Sum256(body)
		source.Fingerprint = hex.EncodeToString(sum[:])
		source.RevisionID = uuid.NewString()
		source.Parser = "safe-text-v1"
		if media == "text/html" {
			source.Parser = "safe-main-text-v3"
		}
		source.Coverage = coverage
		source.Text = text
		source.Title = title
		source.StorageAllowed = true
		source.Status = "parsed"
		if coverage != "complete_text" {
			source.Status = "partial"
		}
		source.Fragments = fragments(text)
		return source, nil
	}
	return source, nil
}

func fragments(text string) []Fragment {
	result := []Fragment{}
	for start := 0; start < len(text); {
		end := min(start+1200, len(text))
		for end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		result = append(result, Fragment{ID: uuid.NewString(), Start: start, End: end, Text: text[start:end]})
		start = end
	}
	return result
}
