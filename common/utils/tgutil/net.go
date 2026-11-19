package tgutil

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
	"github.com/krau/SaveAny-Bot/config"
	"golang.org/x/net/proxy"
)

// httpProxyDialer implements proxy.ContextDialer for HTTP CONNECT proxies
type httpProxyDialer struct {
	proxyURL *url.URL
	forward  proxy.Dialer
}

func (d *httpProxyDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *httpProxyDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyAddr := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		if d.proxyURL.Scheme == "https" {
			proxyAddr = net.JoinHostPort(d.proxyURL.Hostname(), "443")
		} else {
			proxyAddr = net.JoinHostPort(d.proxyURL.Hostname(), "80")
		}
	}

	var conn net.Conn
	var err error
	if ctxDialer, ok := d.forward.(proxy.ContextDialer); ok {
		conn, err = ctxDialer.DialContext(ctx, "tcp", proxyAddr)
	} else {
		conn, err = d.forward.Dial("tcp", proxyAddr)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to connect to proxy: %w", err)
	}

	connectReq := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}

	if d.proxyURL.User != nil {
		username := d.proxyURL.User.Username()
		password, _ := d.proxyURL.User.Password()
		auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+auth)
	}

	if err := connectReq.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to write CONNECT request: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to read CONNECT response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed with status: %s", resp.Status)
	}

	return conn, nil
}

func newProxyDialer(proxyUrl string) (proxy.ContextDialer, error) {
	parsedURL, err := url.Parse(proxyUrl)
	if err != nil {
		return nil, err
	}

	switch parsedURL.Scheme {
	case "http", "https":
		return &httpProxyDialer{
			proxyURL: parsedURL,
			forward:  proxy.Direct,
		}, nil
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(parsedURL, proxy.Direct)
		if err != nil {
			return nil, err
		}
		return dialer.(proxy.ContextDialer), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
	}
}

func NewConfigProxyResolver() (dcs.Resolver, error) {
	// Direct connections: optionally prefer native IPv6 DC addresses (IPv4 stays as fallback).
	// When a proxy is configured, keep the default order since the proxy may not carry IPv6.
	var resolver dcs.Resolver = dcs.Plain(dcs.PlainOptions{})
	if config.C().Telegram.PreferIPv6 {
		resolver = newIPv6FirstResolver(dcs.PlainOptions{PreferIPv6: true})
	}
	if config.C().Proxy != "" {
		// global proxy, which has lower priority
		dialer, err := newProxyDialer(config.C().Proxy)
		if err != nil {
			return nil, err
		}
		resolver = dcs.Plain(dcs.PlainOptions{
			Dial: dialer.DialContext,
		})
	}
	if config.C().Telegram.Proxy.Enable && config.C().Telegram.Proxy.URL != "" {
		dialer, err := newProxyDialer(config.C().Telegram.Proxy.URL)
		if err != nil {
			return nil, err
		}
		resolver = dcs.Plain(dcs.PlainOptions{
			Dial: dialer.DialContext,
		})
	}
	return resolver, nil
}

// ipv6FirstResolver dials only the IPv6 addresses of a DC first and falls back to the
// full list (IPv4 included) if that fails. gotd's own PreferIPv6 merely sorts candidates,
// but the plain resolver then races every candidate, so a faster IPv4 path can still win.
type ipv6FirstResolver struct {
	inner dcs.Resolver
}

func newIPv6FirstResolver(opts dcs.PlainOptions) dcs.Resolver {
	return ipv6FirstResolver{inner: dcs.Plain(opts)}
}

func onlyIPv6(list dcs.List, dc int) (dcs.List, bool) {
	v6 := make([]tg.DCOption, 0, len(list.Options))
	for _, o := range list.Options {
		if o.ID == dc && o.Ipv6 {
			v6 = append(v6, o)
		}
	}
	if len(v6) == 0 {
		return list, false
	}
	list.Options = v6
	return list, true
}

func (r ipv6FirstResolver) try(ctx context.Context, dc int, list dcs.List,
	f func(context.Context, int, dcs.List) (transport.Conn, error)) (transport.Conn, error) {
	if v6, ok := onlyIPv6(list, dc); ok {
		// Bound the IPv6 attempt so a broken IPv6 route falls back to IPv4 quickly.
		v6ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		conn, err := f(v6ctx, dc, v6)
		cancel()
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return f(ctx, dc, list)
}

func (r ipv6FirstResolver) Primary(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	return r.try(ctx, dc, list, r.inner.Primary)
}

func (r ipv6FirstResolver) MediaOnly(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	return r.try(ctx, dc, list, r.inner.MediaOnly)
}

func (r ipv6FirstResolver) CDN(ctx context.Context, dc int, list dcs.List) (transport.Conn, error) {
	return r.try(ctx, dc, list, r.inner.CDN)
}
