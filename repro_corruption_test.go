package zmodem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// Reproduces the bbs 2:5020/2021 <- 2:5020/8912 corruption: a continuously
// streaming sender (KittenMail) over a lossy local serial link drops a ZDLE
// that frames a subpacket end-marker; the receiver merges two subpackets and
// CRC-16's residue property lets the merged frame pass, writing wrong bytes at
// a valid offset (full-length, no error) — caught only later by the TIC CRC.
//
// The merged-subpacket detector must drive this to zero WITHOUT introducing
// false-positive stalls (a legit subpacket whose bytes happen to look like an
// embedded frame must still complete).

// corruptReader flips, DROPS, and DUPLICATES bytes on the sender->receiver
// (data) stream — drops/dups desync ZDLE framing, which a pure bit-flip never
// does. Corruption is applied to a private scratch buffer.
type corruptReader struct {
	r        io.Reader
	rng      *rand.Rand
	everyN   int
	counter  int
	pending  []byte
	nFlipped int
	nDropped int
	nDuped   int
}

func (c *corruptReader) Read(p []byte) (int, error) {
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	n, err := c.r.Read(p)
	if n == 0 {
		return 0, err
	}
	out := make([]byte, 0, n+8)
	for i := 0; i < n; i++ {
		c.counter++
		if c.counter < c.everyN {
			out = append(out, p[i])
			continue
		}
		c.counter = 0
		switch c.rng.Intn(3) {
		case 0:
			out = append(out, p[i]^(1<<uint(c.rng.Intn(8))))
			c.nFlipped++
		case 1:
			c.nDropped++
		case 2:
			out = append(out, p[i], p[i])
			c.nDuped++
		}
	}
	m := copy(p, out)
	if m < len(out) {
		c.pending = append(c.pending, out[m:]...)
	}
	return m, err
}

// verifyingSink is an append-only WriteCloser that flags the first byte written
// that diverges from the known source at the current offset.
type verifyingSink struct {
	source   []byte
	pos      int64
	firstBad int64
	ctx      string
}

func newVerifyingSink(src []byte) *verifyingSink { return &verifyingSink{source: src, firstBad: -1} }

func (v *verifyingSink) Write(p []byte) (int, error) {
	for i := 0; i < len(p); i++ {
		off := v.pos + int64(i)
		if v.firstBad < 0 && off < int64(len(v.source)) && p[i] != v.source[off] {
			v.firstBad = off
			v.ctx = fmt.Sprintf("at offset %d: got 0x%02x want 0x%02x", off, p[i], v.source[off])
		}
	}
	v.pos += int64(len(p))
	return len(p), nil
}
func (v *verifyingSink) Close() error { return nil }

type reproHandler struct {
	mu     sync.Mutex
	toSend []*FileOffer
	idx    int
	sink   *verifyingSink
}

func (h *reproHandler) NextFile() *FileOffer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.idx >= len(h.toSend) {
		return nil
	}
	f := h.toSend[h.idx]
	h.idx++
	return f
}
func (h *reproHandler) AcceptFile(info FileInfo) (io.WriteCloser, int64, error) {
	return h.sink, 0, nil
}
func (h *reproHandler) FileProgress(FileInfo, int64)         {}
func (h *reproHandler) FileCompleted(FileInfo, int64, error) {}

type reproResult struct {
	sink    *verifyingSink
	recvErr error
	cr      *corruptReader
}

func runLossyTransfer(t *testing.T, content []byte, everyN int, seed int64, useCRC32 bool) reproResult {
	t.Helper()
	r1, w1 := bufferedPipe(256)
	r2, w2 := bufferedPipe(256)
	cr := &corruptReader{r: r1, rng: rand.New(rand.NewSource(seed)), everyN: everyN}
	senderT := &pipeReadWriter{Reader: r2, Writer: w1}
	receiverT := &pipeReadWriter{Reader: cr, Writer: w2}
	sink := newVerifyingSink(content)

	sender := &reproHandler{toSend: []*FileOffer{{
		Name: "repro.bin", Size: int64(len(content)),
		ModTime: time.Unix(1700000000, 0), Mode: 0644,
		Reader: bytes.NewReader(content),
	}}}
	receiver := &reproHandler{sink: sink}

	cfg := &Config{
		MaxBlockSize: 8192, DataStallTimeout: 4 * time.Second,
		RecvTimeout: 2 * time.Second, Use32BitCRC: useCRC32,
		DetectMergedSubpackets: true,
	}
	sSess := NewSession(senderT, sender, cfg)
	rSess := NewSession(receiverT, receiver, cfg)
	sSess.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	rSess.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var recvErr error
	wg.Add(2)
	go func() { defer wg.Done(); defer w1.Close(); _ = sSess.Send(ctx) }()
	go func() { defer wg.Done(); defer w2.Close(); recvErr = rSess.Receive(ctx) }()
	wg.Wait()
	return reproResult{sink: sink, recvErr: recvErr, cr: cr}
}

// corruptionSweep runs n lossy streaming transfers; returns (corrupt, complete, stalled).
func corruptionSweep(t *testing.T, content []byte, n int, everyN int, useCRC32 bool) (int, int, int) {
	t.Helper()
	testKittenStreamRecovery = true
	defer func() { testKittenStreamRecovery = false }()
	corrupt, complete, stalled := 0, 0, 0
	for seed := int64(1); seed <= int64(n); seed++ {
		r := runLossyTransfer(t, content, everyN, seed, useCRC32)
		full := r.recvErr == nil && r.sink.pos == int64(len(content))
		status := "ok"
		switch {
		case r.sink.firstBad >= 0:
			status = "CORRUPT " + r.sink.ctx
			corrupt++
		case !full:
			status = fmt.Sprintf("STALL recvErr=%v len=%d/%d", r.recvErr, r.sink.pos, len(content))
			stalled++
		default:
			complete++
		}
		t.Logf("seed=%2d flip=%d drop=%d dup=%d -> %s",
			seed, r.cr.nFlipped, r.cr.nDropped, r.cr.nDuped, status)
	}
	return corrupt, complete, stalled
}

func reproContent() []byte {
	c := make([]byte, 120*1024)
	rand.New(rand.NewSource(99)).Read(c)
	return c
}

// TestReproCRC16 is the regression pin: a streaming CRC-16 transfer over a lossy
// link must never assemble a full-length but byte-wrong file, and must not stall.
func TestReproCRC16(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	corrupt, complete, stalled := corruptionSweep(t, reproContent(), 40, 2500, false)
	t.Logf("CRC-16 SUMMARY: corrupt=%d complete=%d stalled=%d /40", corrupt, complete, stalled)
	if corrupt > 0 {
		t.Fatalf("FAIL: %d/40 CRC-16 transfers assembled a wrong-content file (merge not detected)", corrupt)
	}
	if stalled > 0 {
		t.Fatalf("FAIL: %d/40 CRC-16 transfers stalled (false-positive merge detection?)", stalled)
	}
}

// TestReproCRC32 is the control: CRC-32 was always safe (different residue).
func TestReproCRC32(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	c := make([]byte, 64*1024)
	rand.New(rand.NewSource(99)).Read(c)
	corrupt, _, stalled := corruptionSweep(t, c, 15, 2500, true)
	t.Logf("CRC-32 SUMMARY: corrupt=%d stalled=%d /15", corrupt, stalled)
	if corrupt > 0 || stalled > 0 {
		t.Fatalf("CRC-32 corrupt=%d stalled=%d", corrupt, stalled)
	}
}

// TestDetectMergedSubpacketCRC16 pins the detector against a hand-built merge
// and confirms a clean subpacket is not flagged.
func TestDetectMergedSubpacketCRC16(t *testing.T) {
	mkframe := func(sub1, sub2 []byte, end1 byte) []byte {
		// sub1 + ZDLE-less end-char (lost ZDLE) + sub1's CRC + sub2 == what the
		// receiver de-escapes when the ZDLE before sub1's end-marker is dropped.
		crc := crc16Finalize(crc16Update(crc16Update(0, sub1), []byte{end1}))
		out := append([]byte(nil), sub1...)
		out = append(out, end1, byte(crc>>8), byte(crc&0xff))
		return append(out, sub2...)
	}
	rng := rand.New(rand.NewSource(7))
	for _, end := range []byte{ZCRCE, ZCRCG, ZCRCQ, ZCRCW} {
		sub1 := make([]byte, 100)
		sub2 := make([]byte, 80)
		rng.Read(sub1)
		rng.Read(sub2)
		merged := mkframe(sub1, sub2, end)
		if got := detectMergedSubpacketCRC16(merged); got != len(sub1) {
			t.Errorf("end=0x%02x: detect split=%d, want %d", end, got, len(sub1))
		}
	}
	// A plain random subpacket must (almost surely) not be flagged.
	flagged := 0
	for i := 0; i < 2000; i++ {
		b := make([]byte, 256)
		rng.Read(b)
		if detectMergedSubpacketCRC16(b) >= 0 {
			flagged++
		}
	}
	if flagged > 2 { // ~256*4/256/65536 expected ≈ 0.06 over 2000 → a handful tops
		t.Errorf("false positives on random data: %d/2000 (too many)", flagged)
	}
	t.Logf("random false positives: %d/2000", flagged)
}
