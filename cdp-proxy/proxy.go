package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Chromium's DevTools endpoint rejects requests whose Host header is not an
// IP literal or localhost (anti-DNS-rebinding), so the outbound Host is
// always forced to the loopback upstream. Discovery responses embed that
// loopback address in webSocketDebuggerUrl; those bodies are rewritten back
// to the host the client actually used.
const jsonPathPrefix = "/json"

type originalHostKey struct{}
type cdpRequestKey struct{}

type cdpRequestState struct {
	connection *idleConnection
}

func newProxyHandler(loopbackDevtools, advertised string, monitor *idleMonitor) http.Handler {
	target := &url.URL{Scheme: "http", Host: loopbackDevtools}
	// Compression disabled so buffered /json bodies are always plain text;
	// explicit client Accept-Encoding on other paths still passes through.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if needsBodyRewrite(pr.In.URL.Path) {
				pr.Out.Header.Del("Accept-Encoding")
			}
			host := clientHost(pr.In, advertised)
			pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), originalHostKey{}, host))
		},
		Transport:     &rewritingTransport{rt: transport, internal: loopbackDevtools},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if monitor == nil || resp.StatusCode != http.StatusSwitchingProtocols || !isCDPUpgrade(resp.Request) || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
				return nil
			}
			state, ok := resp.Request.Context().Value(cdpRequestKey{}).(*cdpRequestState)
			if !ok {
				return errors.New("CDP upgrade is missing request state")
			}
			upstream, ok := resp.Body.(io.ReadWriteCloser)
			if !ok {
				return errors.New("CDP upgrade body is not writable")
			}
			conn, err := monitor.register(upstream)
			if err != nil {
				return fmt.Errorf("register CDP connection: %w", err)
			}
			state.connection = conn
			resp.Body = conn
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.WarnContext(r.Context(), "proxy error", "path", r.URL.Path, "err", err)
			if errors.Is(err, errBrowserRestarting) {
				http.Error(w, errBrowserRestarting.Error(), http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if monitor != nil && isCDPUpgrade(r) {
			if monitor.unavailable() {
				http.Error(w, errBrowserRestarting.Error(), http.StatusServiceUnavailable)
				return
			}
			state := &cdpRequestState{}
			r = r.WithContext(context.WithValue(r.Context(), cdpRequestKey{}, state))
			defer func() {
				if state.connection != nil {
					if err := state.connection.Close(); err != nil {
						slog.WarnContext(r.Context(), "close proxied CDP connection", "path", r.URL.Path, "err", err)
					}
				}
			}()
		}
		rp.ServeHTTP(w, r)
	}))
}

func isCDPUpgrade(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/devtools/") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func needsBodyRewrite(path string) bool {
	return len(path) >= len(jsonPathPrefix) && path[:len(jsonPathPrefix)] == jsonPathPrefix
}

func clientHost(r *http.Request, fallback string) string {
	if r.Host != "" {
		return r.Host
	}
	return fallback
}

type rewritingTransport struct {
	rt       http.RoundTripper
	internal string
}

// RoundTrip buffers only successful /json discovery bodies so their embedded
// URLs can be repointed at the client-visible host; everything else streams.
func (t *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.rt.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK || !needsBodyRewrite(req.URL.Path) {
		return resp, err //nolint:wrapcheck // passthrough semantics
	}
	host, _ := req.Context().Value(originalHostKey{}).(string)
	if host == "" || host == t.internal {
		return resp, nil
	}

	bodyBytes, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read discovery response: %w", errors.Join(readErr, closeErr))
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close discovery response: %w", closeErr)
	}
	outBytes := bytes.ReplaceAll(bodyBytes, []byte(t.internal), []byte(host))
	outBytes = bytes.ReplaceAll(outBytes, []byte("localhost:"+portOf(t.internal)), []byte(host))

	resp.Body = io.NopCloser(bytes.NewReader(outBytes))
	resp.ContentLength = int64(len(outBytes))
	resp.Header.Set("Content-Length", strconv.Itoa(len(outBytes)))
	slog.DebugContext(req.Context(), "rewrote devtools json body",
		"host", host,
		"bytes_in", len(bodyBytes),
		"bytes_out", len(outBytes))
	return resp, nil
}

func portOf(hostPort string) string {
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	return port
}

// advertisedHost is the Host value substituted when a client sends none
// (HTTP/1.0); empty for wildcard listeners where no sensible guess exists.
func advertisedHost(listenAddr string) string {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		return ""
	}
	return listenAddr
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		slog.DebugContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ns", time.Since(started))
	})
}
