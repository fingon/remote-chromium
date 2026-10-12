package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"gotest.tools/v3/assert"
)

const (
	testIdleTimeout = 10 * time.Second
	testCommand     = `{"id":1,"method":"Browser.getVersion"}`
	textOpcode      = 1
	pingOpcode      = 9
	pongOpcode      = 10
	closeOpcode     = 8
)

type testClock struct {
	mu   sync.Mutex
	time time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.time
}

func (c *testClock) advance(elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.time = c.time.Add(elapsed)
}

type testConnection struct {
	bytes.Buffer
	closed   bool
	closeErr error
}

func (c *testConnection) Close() error {
	c.closed = true
	return c.closeErr
}

type idleFixture struct {
	monitor  *idleMonitor
	clock    *testClock
	restarts int
}

func newIdleFixture(t *testing.T) *idleFixture {
	t.Helper()
	f := &idleFixture{clock: &testClock{time: time.Unix(0, 0)}}
	f.monitor = newIdleMonitor(testIdleTimeout, f.clock.now, func(context.Context) error {
		f.restarts++
		return nil
	})
	t.Cleanup(f.monitor.shutdown)
	return f
}

func (f *idleFixture) connect(t *testing.T) (*idleConnection, *testConnection) {
	t.Helper()
	upstream := &testConnection{}
	conn, err := f.monitor.register(upstream)
	assert.NilError(t, err)
	f.monitor.mu.Lock()
	assert.Assert(t, conn.activateLocked())
	f.monitor.mu.Unlock()
	return conn, upstream
}

func (f *idleFixture) checkAfter(elapsed time.Duration) {
	f.clock.advance(elapsed)
	f.monitor.check(context.Background())
}

func writeFrame(t *testing.T, conn *idleConnection, opcode byte) {
	t.Helper()
	frame := clientFrame(opcode, []byte(testCommand), true)
	n, err := conn.Write(frame)
	assert.NilError(t, err)
	assert.Equal(t, n, len(frame))
}

func TestIdleMonitorLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *idleFixture)
	}{
		{"startup_restarts_once", func(t *testing.T, f *idleFixture) {
			f.checkAfter(testIdleTimeout - time.Nanosecond)
			assert.Equal(t, f.restarts, 0)
			f.checkAfter(time.Nanosecond)
			assert.Equal(t, f.restarts, 1)
			for range 3 {
				f.checkAfter(testIdleTimeout)
			}
			assert.Equal(t, f.restarts, 1)
		}},
		{"disconnect_starts_full_timeout", func(t *testing.T, f *idleFixture) {
			conn, upstream := f.connect(t)
			f.clock.advance(testIdleTimeout)
			assert.NilError(t, conn.Close())
			assert.Assert(t, upstream.closed)
			f.checkAfter(testIdleTimeout - time.Nanosecond)
			assert.Equal(t, f.restarts, 0)
			f.checkAfter(time.Nanosecond)
			assert.Equal(t, f.restarts, 1)
		}},
		{"all_sessions_idle", func(t *testing.T, f *idleFixture) {
			first, firstUpstream := f.connect(t)
			second, secondUpstream := f.connect(t)
			f.checkAfter(testIdleTimeout)
			assert.Equal(t, f.restarts, 1)
			assert.Assert(t, firstUpstream.closed && secondUpstream.closed)
			_, err := first.Write([]byte(testCommand))
			assert.ErrorIs(t, err, net.ErrClosed)
			_, err = second.Write([]byte(testCommand))
			assert.ErrorIs(t, err, net.ErrClosed)
			f.checkAfter(testIdleTimeout)
			assert.Equal(t, f.restarts, 1)
		}},
		{"one_active_session_postpones_restart", func(t *testing.T, f *idleFixture) {
			f.connect(t)
			active, _ := f.connect(t)
			f.clock.advance(testIdleTimeout / 2)
			writeFrame(t, active, textOpcode)
			f.checkAfter(testIdleTimeout / 2)
			assert.Equal(t, f.restarts, 0)
			f.checkAfter(testIdleTimeout / 2)
			assert.Equal(t, f.restarts, 1)
		}},
		{"new_connection_rearms_idle_restart", func(t *testing.T, f *idleFixture) {
			f.checkAfter(testIdleTimeout)
			f.clock.advance(testIdleTimeout)
			f.connect(t)
			f.checkAfter(testIdleTimeout - time.Nanosecond)
			assert.Equal(t, f.restarts, 1)
			f.checkAfter(time.Nanosecond)
			assert.Equal(t, f.restarts, 2)
			f.checkAfter(testIdleTimeout)
			assert.Equal(t, f.restarts, 2)
		}},
		{"new_connection_then_disconnect_rearms", func(t *testing.T, f *idleFixture) {
			f.checkAfter(testIdleTimeout)
			conn, _ := f.connect(t)
			assert.NilError(t, conn.Close())
			f.checkAfter(testIdleTimeout)
			assert.Equal(t, f.restarts, 2)
		}},
		{"browser_events_do_not_reset_timer", func(t *testing.T, f *idleFixture) {
			conn, upstream := f.connect(t)
			f.clock.advance(testIdleTimeout / 2)
			_, err := upstream.WriteString(testCommand)
			assert.NilError(t, err)
			payload := make([]byte, len(testCommand))
			_, err = io.ReadFull(conn, payload)
			assert.NilError(t, err)
			assert.Equal(t, string(payload), testCommand)
			f.checkAfter(testIdleTimeout / 2)
			assert.Equal(t, f.restarts, 1)
		}},
		{"control_frames_do_not_reset_timer", func(t *testing.T, f *idleFixture) {
			conn, _ := f.connect(t)
			f.clock.advance(testIdleTimeout / 2)
			for _, opcode := range []byte{pingOpcode, pongOpcode, closeOpcode} {
				writeFrame(t, conn, opcode)
			}
			f.checkAfter(testIdleTimeout / 2)
			assert.Equal(t, f.restarts, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newIdleFixture(t))
		})
	}
}

func TestIdleMonitorRestartFailure(t *testing.T) {
	f := newIdleFixture(t)
	f.monitor.restart = func(context.Context) error {
		f.restarts++
		if f.restarts == 1 {
			return errors.New("supervisor unavailable")
		}
		return nil
	}
	f.checkAfter(testIdleTimeout)
	assert.Equal(t, f.restarts, 1)
	f.checkAfter(restartRetryDelay - time.Nanosecond)
	assert.Equal(t, f.restarts, 1)
	f.checkAfter(time.Nanosecond)
	assert.Equal(t, f.restarts, 2)
	f.checkAfter(restartRetryDelay)
	assert.Equal(t, f.restarts, 2)
}

func TestIdleMonitorSerializesRestart(t *testing.T) {
	f := newIdleFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	f.monitor.restart = func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		if !bounded {
			return errors.New("restart context has no deadline")
		}
		close(started)
		<-release
		return nil
	}
	f.clock.advance(testIdleTimeout)
	go func() {
		defer close(done)
		f.monitor.check(context.Background())
	}()
	<-started
	assert.Assert(t, f.monitor.unavailable())
	_, err := f.monitor.register(&testConnection{})
	assert.ErrorIs(t, err, errBrowserRestarting)
	f.monitor.check(context.Background())
	close(release)
	<-done
	assert.Assert(t, !f.monitor.unavailable())
	assert.Assert(t, !f.monitor.eligible)
}

func TestIdleMonitorRetriesFailedRestartOfIdleSessions(t *testing.T) {
	f := newIdleFixture(t)
	f.monitor.timeout = 2 * restartRetryDelay
	f.connect(t)
	f.monitor.restart = func(context.Context) error {
		f.restarts++
		if f.restarts == 1 {
			return errors.New("supervisor unavailable")
		}
		return nil
	}
	f.checkAfter(f.monitor.timeout)
	assert.Equal(t, f.restarts, 1)
	f.checkAfter(restartRetryDelay)
	assert.Equal(t, f.restarts, 2)
	f.checkAfter(f.monitor.timeout)
	assert.Equal(t, f.restarts, 2)
}

func TestIdleMonitorShutdown(t *testing.T) {
	f := newIdleFixture(t)
	conn, upstream := f.connect(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.monitor.run(ctx)
	assert.Assert(t, upstream.closed)
	assert.NilError(t, conn.Close())
	_, err := f.monitor.register(&testConnection{})
	assert.ErrorIs(t, err, errBrowserRestarting)
	f.checkAfter(testIdleTimeout)
	assert.Equal(t, f.restarts, 0)
}

func TestIdleConnectionCloseError(t *testing.T) {
	f := newIdleFixture(t)
	conn, upstream := f.connect(t)
	closeErr := errors.New("close failed")
	upstream.closeErr = closeErr
	assert.ErrorIs(t, conn.Close(), closeErr)
	assert.Equal(t, len(f.monitor.sessions), 0)
}

func clientFrame(opcode byte, payload []byte, masked bool) []byte {
	frame := []byte{0x80 | opcode}
	maskFlag := byte(0)
	if masked {
		maskFlag = websocketMaskFlag
	}
	switch {
	case len(payload) < websocketExtended16:
		frame = append(frame, byte(len(payload))|maskFlag)
	case len(payload) <= 65535:
		frame = append(frame, websocketExtended16|maskFlag)
		frame = binary.BigEndian.AppendUint16(frame, uint16(len(payload)))
	default:
		frame = append(frame, websocketExtended64|maskFlag)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(payload)))
	}
	mask := [websocketMaskBytes]byte{1, 2, 3, 4}
	if masked {
		frame = append(frame, mask[:]...)
	}
	for i, b := range payload {
		if masked {
			b ^= mask[i%websocketMaskBytes]
		}
		frame = append(frame, b)
	}
	return frame
}

func TestWebsocketActivity(t *testing.T) {
	for _, tc := range []struct {
		name         string
		opcode       byte
		payloadBytes int
		masked       bool
		active       bool
	}{
		{"text", textOpcode, len(testCommand), true, true},
		{"binary", 2, len(testCommand), true, true},
		{"continuation", 0, len(testCommand), true, true},
		{"empty", textOpcode, 0, true, false},
		{"ping", pingOpcode, len(testCommand), true, false},
		{"pong", pongOpcode, len(testCommand), true, false},
		{"close", closeOpcode, len(testCommand), true, false},
		{"extended_16", textOpcode, websocketExtended16, true, true},
		{"extended_64", textOpcode, 65536, true, true},
		{"unmasked", textOpcode, len(testCommand), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := clientFrame(tc.opcode, bytes.Repeat([]byte("x"), tc.payloadBytes), tc.masked)
			for _, chunkBytes := range []int{1, 3, len(frame)} {
				parser := websocketActivity{}
				active := false
				for offset := 0; offset < len(frame); offset += chunkBytes {
					observed := parser.consume(frame[offset:min(offset+chunkBytes, len(frame))])
					active = active || observed
				}
				assert.Equal(t, active, tc.active)
				assert.Equal(t, parser.payloadRemainingBytes, uint64(0))
				assert.Equal(t, parser.headerLen, 0)
				assert.Assert(t, !parser.consume(clientFrame(pingOpcode, []byte("ping"), true)))
				assert.Assert(t, parser.consume(clientFrame(textOpcode, []byte(testCommand), true)))
			}
		})
	}
}

func TestIdleConnectionForwardsFragmentedFrames(t *testing.T) {
	f := newIdleFixture(t)
	conn, upstream := f.connect(t)
	frame := clientFrame(textOpcode, []byte(testCommand), true)
	f.clock.advance(testIdleTimeout / 2)
	for _, b := range frame {
		n, err := conn.Write([]byte{b})
		assert.NilError(t, err)
		assert.Equal(t, n, 1)
	}
	assert.DeepEqual(t, upstream.Bytes(), frame)
	f.checkAfter(testIdleTimeout / 2)
	assert.Equal(t, f.restarts, 0)
	f.checkAfter(testIdleTimeout / 2)
	assert.Equal(t, f.restarts, 1)
}

func TestIdleTimeoutConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       string
		args      []string
		want      time.Duration
		wantError bool
	}{
		{"default", "", nil, defaultIdleTimeout, false},
		{"environment", "30s", nil, 30 * time.Second, false},
		{"flag", "", []string{"--idle-timeout", "2m"}, 2 * time.Minute, false},
		{"flag_overrides_environment", "30s", []string{"--idle-timeout", "2m"}, 2 * time.Minute, false},
		{"invalid", "", []string{"--idle-timeout", "invalid"}, 0, true},
		{"zero", "0s", nil, 0, true},
		{"negative", "", []string{"--idle-timeout=-1m"}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CDP_IDLE_TIMEOUT", tc.env)
			if tc.env == "" {
				assert.NilError(t, os.Unsetenv("CDP_IDLE_TIMEOUT"))
			}
			var cfg cli
			parser, err := kong.New(&cfg, kong.Vars{"defaultIdleTimeout": defaultIdleTimeout.String()})
			assert.NilError(t, err)
			_, err = parser.Parse(tc.args)
			if tc.wantError {
				assert.Assert(t, err != nil)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, cfg.IdleTimeout, tc.want)
		})
	}
}
