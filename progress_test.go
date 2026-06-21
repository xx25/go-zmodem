package zmodem

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// fileHandlerStub satisfies FileHandler; recoverData never calls into it.
type fileHandlerStub struct{}

func (fileHandlerStub) NextFile() *FileOffer                               { return nil }
func (fileHandlerStub) AcceptFile(FileInfo) (io.WriteCloser, int64, error) { return nil, 0, ErrSkip }
func (fileHandlerStub) FileProgress(FileInfo, int64)                       {}
func (fileHandlerStub) FileCompleted(FileInfo, int64, error)               {}

// newProbeSession builds a Session writing to buf with a fixed, overridable
// clock, for exercising recoverData's abort criterion in isolation.
func newProbeSession(buf *bytes.Buffer, cfg *Config, clock func() time.Time) *Session {
	s := NewSession(buf, fileHandlerStub{}, cfg)
	s.tr.now = clock
	return s
}

// zrposSent reports whether buf contains a ZRPOS hex header (ZPAD ZPAD ZDLE
// ZHEX then frame type "09").
func zrposSent(buf *bytes.Buffer) bool {
	b := buf.Bytes()
	return bytes.Contains(b, []byte{ZPAD, ZPAD, ZDLE, ZHEX, '0', '9'})
}

// TestRecoverDataProgressAwareAbort: with DataStallTimeout set, recoverData
// continues (sends one ZRPOS) while the transfer is within the stall window, and
// aborts once no progress has been made for the whole window — regardless of how
// many errors occurred.
func TestRecoverDataProgressAwareAbort(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	buf := &bytes.Buffer{}
	s := newProbeSession(buf, &Config{DataStallTimeout: 60 * time.Second, Logger: discardLogger()}, func() time.Time { return now })
	s.lastProgressAt = base

	// Within the window: many recovery cycles, all continue with a ZRPOS.
	retries := 0
	for i := 0; i < 100; i++ {
		now = base.Add(time.Duration(i) * 100 * time.Millisecond) // < 60s
		if err := s.recoverData(0, &retries); err != nil {
			t.Fatalf("cycle %d aborted within stall window: %v", i, err)
		}
	}
	if !zrposSent(buf) {
		t.Fatal("expected a ZRPOS to be sent while recovering within the window")
	}

	// Cross the stall window with no progress → abort.
	now = base.Add(61 * time.Second)
	err := s.recoverData(0, &retries)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("expected stall abort past the window, got %v", err)
	}
}

// TestRecoverDataProgressResetsClock: a valid subpacket (modeled here by
// refreshing lastProgressAt) keeps the transfer alive across an arbitrarily long
// run as long as it keeps advancing.
func TestRecoverDataProgressResetsClock(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	now := base
	buf := &bytes.Buffer{}
	s := newProbeSession(buf, &Config{DataStallTimeout: 60 * time.Second, Logger: discardLogger()}, func() time.Time { return now })
	s.lastProgressAt = base

	retries := 0
	for i := 0; i < 50; i++ {
		now = now.Add(50 * time.Second) // would exceed 60s only if never reset
		s.lastProgressAt = now          // a good subpacket arrived: progress
		if err := s.recoverData(0, &retries); err != nil {
			t.Fatalf("iteration %d aborted despite ongoing progress: %v", i, err)
		}
	}
}

// TestRecoverDataLegacyCountAbort: with DataStallTimeout == 0 the legacy
// consecutive-retry count governs — abort after dataRetryBudget cycles.
func TestRecoverDataLegacyCountAbort(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	buf := &bytes.Buffer{}
	s := newProbeSession(buf, &Config{DataStallTimeout: 0, Logger: discardLogger()}, func() time.Time { return base })
	s.lastProgressAt = base

	retries := 0
	for i := 0; i < dataRetryBudget; i++ {
		if err := s.recoverData(0, &retries); err != nil {
			t.Fatalf("cycle %d aborted before budget exhausted: %v", i, err)
		}
	}
	// One past the budget → abort with the legacy message.
	err := s.recoverData(0, &retries)
	if err == nil || !strings.Contains(err.Error(), "max retries exceeded") {
		t.Fatalf("expected legacy count abort, got %v", err)
	}
}
