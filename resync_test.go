package zmodem

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// defaultGarbageThreshold returns the GarbageThreshold the library applies to
// a zero-value Config, so the resync test tracks the real default instead of
// hardcoding 1200.
func defaultGarbageThreshold() int {
	var c Config
	c.defaults()
	return c.GarbageThreshold
}

// TestScanForPadResetsGarbagePerScan is the regression pin for the mid-stream
// resync failure: scanForPad must give each header hunt a fresh garbage budget.
//
// Layout: a stale run longer than the data-phase retry budget can drain
// one-byte-at-a-time, followed by a valid frame start (ZPAD ZDLE ZHEX).
//
// Without the per-scan reset, scan 1 consumes GarbageThreshold+1 stale bytes
// and overflows, leaving garbageCount latched above the threshold; every later
// scan then overflows on its FIRST byte, draining only one stale byte per
// scan. Reaching the header therefore takes (N-GarbageThreshold) scans — far
// more than dataRetryBudget — so within the budget the receiver actually has,
// the valid header is never found and the transfer aborts in milliseconds.
//
// With the reset, scan 1 drains GarbageThreshold+1 bytes and scan 2 (fresh
// budget) drains the short remainder and lands on the ZPAD, so recovery
// completes within ceil(N/GarbageThreshold)+1 scans — well inside the budget.
func TestScanForPadResetsGarbagePerScan(t *testing.T) {
	threshold := defaultGarbageThreshold()

	// Size the stale run past what dataRetryBudget single-byte scans can drain
	// (see header comment). margin keeps the ZPAD strictly beyond the reach of
	// the pre-fix one-byte-per-scan drain even at the budget's edge.
	const margin = 8
	n := threshold + dataRetryBudget + margin

	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		buf.WriteByte(0xAA) // neither ZPAD (0x2a) nor CAN/ZDLE (0x18): pure garbage
	}
	buf.WriteByte(ZPAD)
	buf.WriteByte(ZDLE)
	buf.WriteByte(ZHEX)

	tr := newTransportReader(&buf, threshold, 0, true, slog.Default())

	// ceil(n/threshold)+1 is the upper bound on scans the fixed code needs:
	// each scan drains up to `threshold` bytes, plus one tail scan to land on
	// the header.
	maxScans := (n+threshold-1)/threshold + 1

	var (
		enc   byte
		err   error
		scans int
	)
	// Loop only as far as the receiver's real data-phase budget: a fix that
	// merely "eventually" recovers but not within the budget is no fix.
	for scans = 1; scans <= dataRetryBudget; scans++ {
		enc, err = tr.scanForPad()
		if err == nil {
			break
		}
		if !errors.Is(err, errGarbageOverflow) {
			t.Fatalf("scan %d: unexpected error: %v (want errGarbageOverflow while draining)", scans, err)
		}
	}

	if err != nil {
		t.Fatalf("scanForPad never found the frame within the %d-retry budget: %v "+
			"(garbageCount latched across scans?)", dataRetryBudget, err)
	}
	if enc != ZHEX {
		t.Fatalf("scanForPad returned encoding 0x%02x, want ZHEX 0x%02x", enc, ZHEX)
	}
	if scans > maxScans {
		t.Fatalf("recovery took %d scans, want <= %d: each scan must reset its garbage budget", scans, maxScans)
	}
}

// TestNewSessionHonoursConfigLogger pins Part B: a logger supplied via
// Config.Logger is used instead of slog.Default().
func TestNewSessionHonoursConfigLogger(t *testing.T) {
	custom := slog.New(slog.NewTextHandler(io.Discard, nil))

	s := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &bytes.Buffer{}}, newTestHandler(),
		&Config{Logger: custom})
	if s.logger != custom {
		t.Fatalf("session logger = %p, want the injected logger %p", s.logger, custom)
	}
	if s.tr.logger != custom {
		t.Fatalf("transportReader logger = %p, want the injected logger %p", s.tr.logger, custom)
	}

	// Nil Config.Logger falls back to slog.Default().
	s2 := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &bytes.Buffer{}}, newTestHandler(),
		&Config{})
	if s2.logger != slog.Default() {
		t.Fatalf("nil Config.Logger should fall back to slog.Default()")
	}
}

// TestDefaultRecvTimeout pins Part B: the exported constant matches the timeout
// NewSession applies on the nil-Config path, so callers synthesizing a Config
// can reproduce it exactly.
func TestDefaultRecvTimeout(t *testing.T) {
	if DefaultRecvTimeout != 10*time.Second {
		t.Fatalf("DefaultRecvTimeout = %v, want 10s", DefaultRecvTimeout)
	}

	// nil Config → the default read timeout is applied.
	s := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &bytes.Buffer{}}, newTestHandler(), nil)
	if s.cfg.RecvTimeout != DefaultRecvTimeout {
		t.Fatalf("nil Config RecvTimeout = %v, want DefaultRecvTimeout %v", s.cfg.RecvTimeout, DefaultRecvTimeout)
	}
	if s.tr.timeout != DefaultRecvTimeout {
		t.Fatalf("nil Config transportReader timeout = %v, want %v", s.tr.timeout, DefaultRecvTimeout)
	}

	// Explicit Config{RecvTimeout: 0} means "disabled" — unchanged by the default.
	s2 := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &bytes.Buffer{}}, newTestHandler(),
		&Config{RecvTimeout: 0})
	if s2.cfg.RecvTimeout != 0 {
		t.Fatalf("explicit RecvTimeout:0 = %v, want 0 (disabled)", s2.cfg.RecvTimeout)
	}
}
