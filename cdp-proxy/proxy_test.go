package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"
)

type upstreamFixture struct {
	server   *httptest.Server
	hostPort string

	mu            sync.Mutex
	lastHost      string
	lastAcceptEnc string
	versionHits   int
}

const (
	versionBodyFmt = `{"Browser":"Chromium/126","webSocketDebuggerUrl":"ws://%s/devtools/browser/guid-123"}`
	appJSBodyFmt   = `self.internal="http://%s/devtools/x.js";`
	listBodyFmt    = `[{"url":"http://other.example/page","title":"%s"}]`
)

func newUpstreamFixture(t *testing.T) *upstreamFixture {
	t.Helper()
	fx := &upstreamFixture{}

	record := func(r *http.Request) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.lastHost = r.Host
		fx.lastAcceptEnc = r.Header.Get("Accept-Encoding")
	}

	internal := func() string {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.hostPort
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		fx.mu.Lock()
		fx.versionHits++
		fx.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, versionBodyFmt, internal())
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, listBodyFmt, internal())
	})
	mux.HandleFunc("/devtools/app.js", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, appJSBodyFmt, internal())
	})
	mux.HandleFunc("/devtools/ws", func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hijacker.Hijack()
		assert.NilError(t, err)
		defer conn.Close()
		if buffered := rw.Reader.Buffered(); buffered > 0 {
			_, _ = io.CopyN(io.Discard, rw.Reader, int64(buffered))
		}
		out := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
		_, _ = conn.Write([]byte(out))
		_, _ = io.Copy(conn, conn)
	})

	fx.server = httptest.NewServer(mux)
	t.Cleanup(fx.server.Close)
	fx.hostPort = strings.TrimPrefix(fx.server.URL, "http://")
	return fx
}

func (fx *upstreamFixture) observed() (host string, acceptEnc string) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.lastHost, fx.lastAcceptEnc
}

func newProxyFixture(t *testing.T, fx *upstreamFixture, advertised string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newProxyHandler(fx.hostPort, advertised))
	t.Cleanup(srv.Close)
	return srv
}

func getViaProxy(t *testing.T, proxyURL, path, reqHost, acceptEnc string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxyURL+path, nil)
	assert.NilError(t, err)
	req.Host = reqHost
	if acceptEnc != "" {
		req.Header.Set("Accept-Encoding", acceptEnc)
	}
	resp, err := http.DefaultClient.Do(req)
	assert.NilError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	assert.NilError(t, err)
	return resp, string(body)
}

func TestRewritesDiscoveryBodyToClientHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
	}{
		{"host_with_port", "fw.lan.example:19222"},
		{"bare_host", "fw.lan.example"},
		{"ipv6_host", "[fd00::1]:19222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newUpstreamFixture(t)
			proxy := newProxyFixture(t, fx, "")

			resp, body := getViaProxy(t, proxy.URL, "/json/version", tc.host, "gzip")

			assert.Equal(t, resp.StatusCode, http.StatusOK)
			assert.Assert(t, strings.Contains(body, tc.host),
				"body %q missing rewritten host %q", body, tc.host)
			assert.Assert(t, !strings.Contains(body, fx.hostPort),
				"body %q still contains internal address %q", body, fx.hostPort)

			upstreamHost, upstreamAcceptEnc := fx.observed()
			assert.Equal(t, upstreamHost, fx.hostPort, "outbound Host not forced to loopback upstream")
			assert.Equal(t, upstreamAcceptEnc, "", "Accept-Encoding should be stripped on buffered paths")
		})
	}
}

func TestNonJSONPassthroughUntouched(t *testing.T) {
	fx := newUpstreamFixture(t)
	proxy := newProxyFixture(t, fx, "")

	wantBody := fmt.Sprintf(appJSBodyFmt, fx.hostPort)
	resp, body := getViaProxy(t, proxy.URL, "/devtools/app.js", "fw.lan.example", "gzip")

	assert.Equal(t, resp.StatusCode, http.StatusOK)
	assert.Equal(t, body, wantBody, "non-JSON body must pass through byte-identical")

	_, upstreamAcceptEnc := fx.observed()
	assert.Equal(t, upstreamAcceptEnc, "gzip", "Accept-Encoding must be preserved when not buffering")
}

func TestErrorStatusNotRewritten(t *testing.T) {
	fx := newUpstreamFixture(t)
	proxy := newProxyFixture(t, fx, "")

	resp, body := getViaProxy(t, proxy.URL, "/json/list", "fw.lan.example", "")

	assert.Equal(t, resp.StatusCode, http.StatusServiceUnavailable, "status passthrough")
	assert.Assert(t, strings.Contains(body, fx.hostPort),
		"non-200 bodies must not be rewritten: %q", body)
}

func TestMissingHostFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		advertised string
		expectIn   string
	}{
		{"wildcard_listener_skips_rewrite", "", ""},
		{"explicit_listener_used_as_fallback", "10.0.0.5:9222", "10.0.0.5:9222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newUpstreamFixture(t)
			handler := newProxyHandler(fx.hostPort, tc.advertised)

			req := httptest.NewRequest(http.MethodGet, "/json/version", nil)
			req.Host = ""
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			body := rec.Body.String()
			if tc.expectIn == "" {
				assert.Assert(t, strings.Contains(body, fx.hostPort),
					"body should stay untouched: %q", body)
			} else {
				assert.Assert(t, strings.Contains(body, tc.expectIn),
					"body %q should contain fallback %q", body, tc.expectIn)
			}
		})
	}
}

func TestWebSocketUpgradePipedThrough(t *testing.T) {
	fx := newUpstreamFixture(t)
	proxy := newProxyFixture(t, fx, "")

	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	assert.NilError(t, err)
	defer conn.Close()
	assert.NilError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	request := "GET /devtools/ws HTTP/1.1\r\n" +
		"Host: fw.lan.example\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	_, err = conn.Write([]byte(request))
	assert.NilError(t, err)

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	assert.NilError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusSwitchingProtocols, "upgrade must survive the proxy")

	sent := "ping-payload-123"
	_, err = conn.Write([]byte(sent))
	assert.NilError(t, err)
	echoed := make([]byte, len(sent))
	_, err = io.ReadFull(reader, echoed)
	assert.NilError(t, err)
	assert.Equal(t, string(echoed), sent, "upgraded connection must be a transparent pipe")
}
