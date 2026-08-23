package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

// Chromium's DevTools endpoint rejects requests whose Host header is not an
// IP literal or localhost (anti-DNS-rebinding), so the outbound Host is
// always forced to the loopback upstream. Discovery responses embed that
// loopback address in webSocketDebuggerUrl; those bodies are rewritten back
// to the host the client actually used.
const jsonPathPrefix = "/json"

type originalHostKey struct{}

func newProxyHandler(loopbackDevtools, advertised string) http.Handler {
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
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.WarnContext(r.Context(), "proxy error", "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return logRequests(rp)
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
	resp.Body.Close()
	if readErr != nil {
		return nil, readErr
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
