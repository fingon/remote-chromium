package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"sync"
	"time"
)

const (
	idleCheckInterval        = time.Second
	restartRetryDelay        = time.Minute
	restartTimeout           = 30 * time.Second
	supervisorConfig         = "/etc/supervisor/supervisord.conf"
	websocketHeaderBytes     = 2
	websocketMaskBytes       = 4
	websocketLengthMask      = 0x7f
	websocketMaskFlag        = 0x80
	websocketExtended16      = 126
	websocketExtended64      = 127
	websocketExtended16Bytes = 2
	websocketExtended64Bytes = 8
	websocketBinaryOpcode    = 2
)

var errBrowserRestarting = errors.New("browser is restarting")

type idleMonitor struct {
	mu             sync.Mutex
	timeout        time.Duration
	now            func() time.Time
	restart        func(context.Context) error
	sessions       map[*idleConnection]time.Time
	noSessionSince time.Time
	retryAt        time.Time
	eligible       bool
	restarting     bool
	closed         bool
}

func newIdleMonitor(timeout time.Duration, now func() time.Time, restart func(context.Context) error) *idleMonitor {
	return &idleMonitor{
		timeout:        timeout,
		now:            now,
		restart:        restart,
		sessions:       make(map[*idleConnection]time.Time),
		noSessionSince: now(),
		eligible:       true,
	}
}

func (m *idleMonitor) register(upstream io.ReadWriteCloser) (*idleConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restarting || m.closed {
		return nil, errBrowserRestarting
	}
	conn := &idleConnection{upstream: upstream, monitor: m}
	m.sessions[conn] = m.now()
	return conn, nil
}

func (m *idleMonitor) unavailable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restarting || m.closed
}

func (m *idleMonitor) run(ctx context.Context) {
	defer m.shutdown()
	ticker := time.NewTicker(min(m.timeout, idleCheckInterval))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check(ctx)
		}
	}
}

func (m *idleMonitor) check(ctx context.Context) {
	m.mu.Lock()
	now := m.now()
	if ctx.Err() != nil || m.closed || m.restarting || !m.eligible || now.Before(m.retryAt) {
		m.mu.Unlock()
		return
	}
	reason := "no_sessions"
	sessionCount := 0
	idleSince := m.noSessionSince
	for conn, lastActivity := range m.sessions {
		if !conn.active {
			continue
		}
		if sessionCount == 0 || lastActivity.After(idleSince) {
			idleSince = lastActivity
		}
		sessionCount++
		if now.Sub(lastActivity) < m.timeout {
			m.mu.Unlock()
			return
		}
	}
	if sessionCount == 0 {
		if now.Sub(m.noSessionSince) < m.timeout {
			m.mu.Unlock()
			return
		}
	} else {
		reason = "all_sessions_idle"
	}
	m.restarting = true
	connections := m.connectionsLocked()
	m.mu.Unlock()

	slog.InfoContext(ctx, "restarting idle browser", "reason", reason, "sessions", sessionCount, "idle_timeout", m.timeout)
	m.closeConnections(connections)
	restartCtx, cancel := context.WithTimeout(ctx, restartTimeout)
	err := m.restart(restartCtx)
	cancel()

	m.mu.Lock()
	m.restarting = false
	if err == nil {
		m.eligible = false
		m.noSessionSince = m.now()
		m.retryAt = time.Time{}
	} else {
		m.noSessionSince = idleSince
		m.retryAt = m.now().Add(restartRetryDelay)
	}
	m.mu.Unlock()
	if err != nil {
		slog.ErrorContext(ctx, "idle browser restart failed", "err", err, "retry_delay", restartRetryDelay)
		return
	}
	slog.InfoContext(ctx, "idle browser restarted; waiting for a new CDP connection")
}

func (m *idleMonitor) connectionsLocked() []*idleConnection {
	connections := make([]*idleConnection, 0, len(m.sessions))
	for conn := range m.sessions {
		connections = append(connections, conn)
	}
	return connections
}

func (m *idleMonitor) closeConnections(connections []*idleConnection) {
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			slog.Error("close CDP connection", "err", err)
		}
	}
}

func (m *idleMonitor) shutdown() {
	m.mu.Lock()
	m.closed = true
	connections := m.connectionsLocked()
	m.mu.Unlock()
	m.closeConnections(connections)
}

type idleConnection struct {
	upstream  io.ReadWriteCloser
	monitor   *idleMonitor
	frames    websocketActivity
	closeOnce sync.Once
	closeErr  error
	active    bool
}

func (c *idleConnection) Read(p []byte) (n int, err error) {
	c.monitor.mu.Lock()
	if !c.activateLocked() {
		c.monitor.mu.Unlock()
		return 0, net.ErrClosed
	}
	c.monitor.mu.Unlock()
	n, err = c.upstream.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		slog.Warn("read CDP connection", "err", err)
	}
	return n, err
}

func (c *idleConnection) Write(p []byte) (n int, err error) {
	c.monitor.mu.Lock()
	if !c.activateLocked() {
		c.monitor.mu.Unlock()
		return 0, net.ErrClosed
	}
	if c.frames.consume(p) {
		c.monitor.sessions[c] = c.monitor.now()
	}
	c.monitor.mu.Unlock()
	n, err = c.upstream.Write(p)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("write CDP connection", "err", err)
	}
	return n, err
}

// ReverseProxy starts copying only after flushing the successful client handshake.
func (c *idleConnection) activateLocked() bool {
	if _, exists := c.monitor.sessions[c]; !exists || c.monitor.restarting || c.monitor.closed {
		return false
	}
	if !c.active {
		c.active = true
		c.monitor.sessions[c] = c.monitor.now()
		c.monitor.eligible = true
		c.monitor.retryAt = time.Time{}
	}
	return true
}

func (c *idleConnection) Close() error {
	c.closeOnce.Do(func() {
		c.monitor.mu.Lock()
		delete(c.monitor.sessions, c)
		activeSessionExists := false
		for conn := range c.monitor.sessions {
			activeSessionExists = activeSessionExists || conn.active
		}
		if c.active && !activeSessionExists {
			c.monitor.noSessionSince = c.monitor.now()
		}
		c.monitor.mu.Unlock()
		if err := c.upstream.Close(); err != nil {
			c.closeErr = fmt.Errorf("close upstream CDP connection: %w", err)
			slog.Warn("close upstream CDP connection", "err", err)
		}
	})
	return c.closeErr
}

// Only frame headers are inspected; masking and payload bytes stay untouched.
type websocketActivity struct {
	header                [websocketHeaderBytes + websocketExtended64Bytes + websocketMaskBytes]byte
	headerLen             int
	payloadRemainingBytes uint64
	dataFrame             bool
}

func (f *websocketActivity) consume(p []byte) bool {
	active := false
	for len(p) > 0 {
		if f.payloadRemainingBytes > 0 {
			n := min(uint64(len(p)), f.payloadRemainingBytes)
			active = active || f.dataFrame
			f.payloadRemainingBytes -= n
			p = p[n:]
			continue
		}
		f.header[f.headerLen] = p[0]
		f.headerLen++
		p = p[1:]
		if f.headerLen < websocketHeaderBytes {
			continue
		}
		length := uint64(f.header[1] & websocketLengthMask)
		headerBytes := websocketHeaderBytes
		switch length {
		case websocketExtended16:
			headerBytes += websocketExtended16Bytes
		case websocketExtended64:
			headerBytes += websocketExtended64Bytes
		}
		if f.header[1]&websocketMaskFlag != 0 {
			headerBytes += websocketMaskBytes
		}
		if f.headerLen < headerBytes {
			continue
		}
		switch length {
		case websocketExtended16:
			length = uint64(binary.BigEndian.Uint16(f.header[websocketHeaderBytes:]))
		case websocketExtended64:
			length = binary.BigEndian.Uint64(f.header[websocketHeaderBytes:])
		}
		opcode := f.header[0] & 0x0f
		f.dataFrame = opcode <= websocketBinaryOpcode
		f.payloadRemainingBytes = length
		f.headerLen = 0
	}
	return active
}

func restartChromium(ctx context.Context) error {
	slog.DebugContext(ctx, "requesting Chromium restart from supervisor")
	cmd := exec.CommandContext(ctx, "supervisorctl", "-c", supervisorConfig, "restart", "chromium")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart Chromium with supervisor: %w (output: %s)", err, output)
	}
	slog.DebugContext(ctx, "supervisor restarted Chromium", "output", string(output))
	return nil
}
