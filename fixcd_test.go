package zmodem

import (
	"bytes"
	"testing"
	"time"
)

// --- Fix C: phase-aware (data-phase) read timeout ---------------------------

func TestActiveTimeoutSelectsPhase(t *testing.T) {
	tr := newTransportReader(&bytes.Buffer{}, 1200, 3*time.Second, true, discardLogger())
	tr.dataTimeout = 30 * time.Second

	if got := tr.activeTimeout(); got != 3*time.Second {
		t.Fatalf("control-phase activeTimeout = %v, want 3s", got)
	}
	tr.setDataPhase(true)
	if got := tr.activeTimeout(); got != 30*time.Second {
		t.Fatalf("data-phase activeTimeout = %v, want 30s", got)
	}
	tr.setDataPhase(false)
	if got := tr.activeTimeout(); got != 3*time.Second {
		t.Fatalf("control-phase activeTimeout after reset = %v, want 3s", got)
	}

	// dataTimeout 0 means "use the control timeout" even in the data phase.
	tr.dataTimeout = 0
	tr.setDataPhase(true)
	if got := tr.activeTimeout(); got != 3*time.Second {
		t.Fatalf("dataTimeout=0 in data phase = %v, want fallback to 3s", got)
	}
}

func TestNewSessionSeedsDataTimeout(t *testing.T) {
	s := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &bytes.Buffer{}}, newTestHandler(),
		&Config{RecvTimeout: 5 * time.Second, DataRecvTimeout: 25 * time.Second})
	if s.tr.dataTimeout != 25*time.Second {
		t.Fatalf("transportReader dataTimeout = %v, want 25s", s.tr.dataTimeout)
	}
}

// infiniteReader yields data forever and records the read deadline set before
// each blocking read, so a test can observe which phase timeout readByte applied.
type infiniteReader struct {
	lastDeadline time.Time
}

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'B'
	}
	return len(p), nil
}

func (r *infiniteReader) SetReadDeadline(t time.Time) error {
	r.lastDeadline = t
	return nil
}

func TestReadByteAppliesPhaseTimeout(t *testing.T) {
	// Control phase: the first read (buffer empty) sets a ~4s deadline.
	ctl := &infiniteReader{}
	trCtl := newTransportReader(ctl, 1200, 4*time.Second, true, discardLogger())
	trCtl.dataTimeout = 40 * time.Second
	before := time.Now()
	if _, err := trCtl.readByte(); err != nil {
		t.Fatalf("control readByte: %v", err)
	}
	if gap := ctl.lastDeadline.Sub(before); gap < 3*time.Second || gap > 6*time.Second {
		t.Fatalf("control-phase deadline gap = %v, want ~4s", gap)
	}

	// Data phase: the first read sets the longer ~40s deadline.
	data := &infiniteReader{}
	trData := newTransportReader(data, 1200, 4*time.Second, true, discardLogger())
	trData.dataTimeout = 40 * time.Second
	trData.setDataPhase(true)
	before = time.Now()
	if _, err := trData.readByte(); err != nil {
		t.Fatalf("data readByte: %v", err)
	}
	if gap := data.lastDeadline.Sub(before); gap < 38*time.Second || gap > 42*time.Second {
		t.Fatalf("data-phase deadline gap = %v, want ~40s", gap)
	}
}

// --- Fix D: receiver attention-sequence transmission ------------------------

// breakRW is a transport that can assert a line break, exercising the AttnBreak
// meta-byte path.
type breakRW struct {
	r     bytes.Buffer
	w     *bytes.Buffer
	broke bool
}

func (b *breakRW) Read(p []byte) (int, error)  { return b.r.Read(p) }
func (b *breakRW) Write(p []byte) (int, error) { return b.w.Write(p) }
func (b *breakRW) SendBreak() error            { b.broke = true; return nil }

func newAttnSession(out *bytes.Buffer, attn []byte) *Session {
	return NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: out}, newTestHandler(),
		&Config{AttnSequence: attn})
}

func TestSendAttnNoopWhenUnset(t *testing.T) {
	var out bytes.Buffer
	s := newAttnSession(&out, nil)
	if s.attnSeq != nil && len(s.attnSeq) != 0 {
		t.Fatalf("attnSeq = %v, want empty", s.attnSeq)
	}
	if err := s.sendAttn(); err != nil {
		t.Fatalf("sendAttn: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("sendAttn wrote %d bytes with no attention sequence set, want 0", out.Len())
	}
}

func TestSendAttnWritesLiteralBytes(t *testing.T) {
	var out bytes.Buffer
	attn := []byte{0x01, 0x02, 'A'}
	s := newAttnSession(&out, attn)
	if !bytes.Equal(s.attnSeq, attn) {
		t.Fatalf("attnSeq = %v, want %v (seeded from Config)", s.attnSeq, attn)
	}
	if err := s.sendAttn(); err != nil {
		t.Fatalf("sendAttn: %v", err)
	}
	if !bytes.Equal(out.Bytes(), attn) {
		t.Fatalf("sendAttn wrote %v, want %v (raw, unescaped)", out.Bytes(), attn)
	}
}

func TestSendAttnBreakAsserted(t *testing.T) {
	brw := &breakRW{w: &bytes.Buffer{}}
	s := NewSession(brw, newTestHandler(), &Config{AttnSequence: []byte{'X', AttnBreak, 'Y'}})
	if err := s.sendAttn(); err != nil {
		t.Fatalf("sendAttn: %v", err)
	}
	if !brw.broke {
		t.Fatal("AttnBreak meta-byte did not assert a line break")
	}
	if !bytes.Equal(brw.w.Bytes(), []byte{'X', 'Y'}) {
		t.Fatalf("sendAttn wrote %v, want [X Y] (break byte must not appear literally)", brw.w.Bytes())
	}
}

func TestSendAttnBreakSkippedWhenUnsupported(t *testing.T) {
	var out bytes.Buffer
	s := newAttnSession(&out, []byte{'X', AttnBreak, 'Y'})
	if err := s.sendAttn(); err != nil {
		t.Fatalf("sendAttn: %v", err)
	}
	// Transport can't break: the break is skipped, the literal bytes still go out.
	if !bytes.Equal(out.Bytes(), []byte{'X', 'Y'}) {
		t.Fatalf("sendAttn wrote %v, want [X Y]", out.Bytes())
	}
}

func TestSendAttnPauses(t *testing.T) {
	var out bytes.Buffer
	s := newAttnSession(&out, []byte{'A', AttnPause, 'B'})
	start := time.Now()
	if err := s.sendAttn(); err != nil {
		t.Fatalf("sendAttn: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("AttnPause elapsed %v, want a ~1s pause", elapsed)
	}
	if !bytes.Equal(out.Bytes(), []byte{'A', 'B'}) {
		t.Fatalf("sendAttn wrote %v, want [A B] (pause byte must not appear literally)", out.Bytes())
	}
}
