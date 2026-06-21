package zmodem

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// --- deadline-capable test transports ---------------------------------------
//
// The quiesce-drain recovery only engages on a transport that implements
// SetReadDeadline (a real modem/net.Conn). The in-memory loopback pipes used by
// the other tests deliberately do NOT, so these tests supply their own
// deadline-capable transports.

// netTimeoutErr is a net.Error whose Timeout() is true, mimicking the modem
// ModemConn read-deadline expiry that drainToQuiet reads as "stream quiescent".
type netTimeoutErr struct{}

func (netTimeoutErr) Error() string   { return "i/o timeout" }
func (netTimeoutErr) Timeout() bool   { return true }
func (netTimeoutErr) Temporary() bool { return true }

// scriptReader is a deadline-capable reader for the drainToQuiet unit tests. It
// returns the queued chunks in order, then `tail` (a non-timeout error) or, if
// tail is nil, a timeout error signalling quiescence. When flood is non-nil it
// ignores chunks and returns that slice on every read forever (a never-quiet
// sender). SetReadDeadline is a no-op: timing is driven by the script (and, for
// the wall-cap test, by an injected clock), not by real deadlines.
type scriptReader struct {
	chunks [][]byte
	idx    int
	tail   error
	flood  []byte
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if r.flood != nil {
		return copy(p, r.flood), nil
	}
	if r.idx < len(r.chunks) {
		c := r.chunks[r.idx]
		r.idx++
		return copy(p, c), nil
	}
	if r.tail != nil {
		return 0, r.tail
	}
	return 0, netTimeoutErr{}
}

func (r *scriptReader) SetReadDeadline(time.Time) error { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDrainToQuietStopsOnQuiescence(t *testing.T) {
	r := &scriptReader{chunks: [][]byte{
		make([]byte, 256), make([]byte, 256), make([]byte, 512),
	}}
	tr := newTransportReader(r, 1200, time.Second, true, discardLogger())

	discarded, err := tr.drainToQuiet(time.Second, 1<<20, time.Minute)
	if err != nil {
		t.Fatalf("drainToQuiet returned error on a stream that goes quiet: %v", err)
	}
	if discarded != 1024 {
		t.Fatalf("discarded = %d, want 1024 (256+256+512 before the quiet gap)", discarded)
	}
}

func TestDrainToQuietHitsByteCap(t *testing.T) {
	r := &scriptReader{flood: make([]byte, 256)} // never goes quiet
	tr := newTransportReader(r, 1200, time.Second, true, discardLogger())

	discarded, err := tr.drainToQuiet(time.Second, 4096, time.Minute)
	if !errors.Is(err, errDrainByteCap) {
		t.Fatalf("err = %v, want errDrainByteCap (sender streaming without re-framing)", err)
	}
	if discarded < 4096 {
		t.Fatalf("discarded = %d, want >= the 4096 byte cap", discarded)
	}
}

func TestDrainToQuietHitsWallCap(t *testing.T) {
	r := &scriptReader{flood: make([]byte, 16)} // tiny chunks: byte cap stays out of reach
	tr := newTransportReader(r, 1200, time.Second, true, discardLogger())

	// Injected clock: each call advances 30ms. With a 100ms wall cap the drain
	// must stop on the wall, not the (1 GiB) byte cap — deterministically and
	// without sleeping.
	base := time.Unix(1000, 0)
	ticks := 0
	tr.now = func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * 30 * time.Millisecond)
	}

	_, err := tr.drainToQuiet(time.Second, 1<<30, 100*time.Millisecond)
	if !errors.Is(err, errDrainWallCap) {
		t.Fatalf("err = %v, want errDrainWallCap (sub-gap trickle that never trips the byte cap)", err)
	}
}

func TestDrainToQuietPropagatesTransportError(t *testing.T) {
	carrierLost := errors.New("modem: carrier lost (DCD dropped)")
	r := &scriptReader{chunks: [][]byte{make([]byte, 100)}, tail: carrierLost}
	tr := newTransportReader(r, 1200, time.Second, true, discardLogger())

	discarded, err := tr.drainToQuiet(time.Second, 1<<20, time.Minute)
	if !errors.Is(err, carrierLost) {
		t.Fatalf("err = %v, want the carrier-loss error propagated (non-timeout aborts the drain)", err)
	}
	if errors.Is(err, errDrainByteCap) || errors.Is(err, errDrainWallCap) {
		t.Fatalf("a transport fault must not be reported as a cap hit: %v", err)
	}
	if discarded != 100 {
		t.Fatalf("discarded = %d, want 100 (the one chunk read before the fault)", discarded)
	}
}

// deadlineConn is a buffered, deadline-capable transport for the receiver-level
// tests: Reads block until a byte arrives or the read deadline expires (then a
// net timeout error), and Writes go to the paired peer reader. Only the receiver
// goroutine touches it, so no locking is needed beyond the channel itself.
type deadlineConn struct {
	in       chan []byte
	inBuf    []byte
	out      io.Writer
	deadline time.Time
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	if len(c.inBuf) > 0 {
		n := copy(p, c.inBuf)
		c.inBuf = c.inBuf[n:]
		return n, nil
	}
	var timer <-chan time.Time
	if !c.deadline.IsZero() {
		d := time.Until(c.deadline)
		if d <= 0 {
			return 0, netTimeoutErr{}
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case data, ok := <-c.in:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		if n < len(data) {
			c.inBuf = data[n:]
		}
		return n, nil
	case <-timer:
		return 0, netTimeoutErr{}
	}
}

func (c *deadlineConn) Write(p []byte) (int, error)       { return c.out.Write(p) }
func (c *deadlineConn) SetReadDeadline(t time.Time) error { c.deadline = t; return nil }

// recordingWriter tees everything written to it into a buffer so a test can
// count frames the receiver sent (e.g. how many ZRPOS hit the wire).
type recordingWriter struct {
	w    io.Writer
	mu   sync.Mutex
	sink bytes.Buffer
}

func (rw *recordingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	rw.sink.Write(p)
	rw.mu.Unlock()
	return rw.w.Write(p)
}

// zrposCount counts ZRPOS hex headers: ZPAD ZPAD ZDLE ZHEX then the two hex
// digits "09" (ZRPOS = 0x09). ZRINIT/ZFIN/etc. carry different type digits and
// are not matched.
func (rw *recordingWriter) zrposCount() int {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return bytes.Count(rw.sink.Bytes(), []byte{ZPAD, ZPAD, ZDLE, ZHEX, '0', '9'})
}

// stoppableWriter writes to a channel but unblocks on `stop`, so a continuously
// flooding peer goroutine can be torn down once the receiver has aborted.
type stoppableWriter struct {
	ch   chan []byte
	stop chan struct{}
}

func (w *stoppableWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case w.ch <- buf:
		return len(p), nil
	case <-w.stop:
		return 0, io.ErrClosedPipe
	}
}

// TestReceiverQuiesceDrainConverges pins the quiesce-drain success path: after a
// mid-stream CRC error the receiver drains the in-flight backlog until the
// sender pauses, sends EXACTLY ONE ZRPOS into the silence (not a re-request per
// scan), the sender re-frames at that offset, and the file converges
// byte-identical.
func TestReceiverQuiesceDrainConverges(t *testing.T) {
	inCh := make(chan []byte, 4096) // peer -> receiver
	rr, rw := bufferedPipe(4096)    // receiver -> peer
	rec := &recordingWriter{w: rw}
	recvConn := &deadlineConn{in: inCh, out: rec}

	// Corrupt the 4th data subpacket's CRC; subsequent writes pass through, so
	// the re-framed resend is clean.
	cw := &corruptingWriter{w: &chanWriter{ch: inCh}, targetCount: 4}
	peerT := &pipeReadWriter{Reader: rr, Writer: cw}

	const (
		blk    = 256
		total  = 4096
		badOff = 3 * blk // bytes written before the corrupt packet
	)
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i*31 + 7)
	}

	recvHandler := newTestHandler()
	receiver := NewSession(recvConn, recvHandler, &Config{
		MaxBlockSize:         blk,
		RecvTimeout:          2 * time.Second,
		DataRecoveryQuietGap: 150 * time.Millisecond,
	})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: blk})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var recvErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		recvErr = receiver.Receive(ctx)
	}()

	// Handshake + file offer.
	mustRecvType(t, peer, ZRINIT, "initial ZRINIT")
	fh := makeHeader(ZFILE)
	fh.SetZF0(ZCBIN)
	if err := peer.sendBinHeader(fh); err != nil {
		t.Fatalf("send ZFILE: %v", err)
	}
	meta := marshalFileInfo(&FileOffer{Name: "drain.bin", Size: total}, 0, 0)
	if err := peer.sendSubpacket(meta, ZCRCW); err != nil {
		t.Fatalf("send ZFILE metadata: %v", err)
	}
	zr := mustRecvType(t, peer, ZRPOS, "ZRPOS after ZFILE")
	if zr.Position() != 0 {
		t.Fatalf("initial ZRPOS pos=%d, want 0", zr.Position())
	}

	// Stream [0, 4*blk): packets 0..2 good, packet 3 corrupt. Then two stale
	// packets the recovery must drain, then go quiet (block on the recovery
	// ZRPOS read).
	if err := peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, 0)); err != nil {
		t.Fatalf("send ZDATA: %v", err)
	}
	for k := 0; k < 4; k++ { // packet 3 (the 4th ZCRCG) gets its CRC corrupted
		off := k * blk
		if err := peer.sendSubpacket(content[off:off+blk], ZCRCG); err != nil {
			t.Fatalf("send subpacket %d: %v", k, err)
		}
	}
	for k := 4; k < 6; k++ { // stale window the drain discards
		off := k * blk
		if err := peer.sendSubpacket(content[off:off+blk], ZCRCG); err != nil {
			t.Fatalf("send stale subpacket %d: %v", k, err)
		}
	}

	// Exactly one ZRPOS, at the good-data boundary.
	zr2 := mustRecvType(t, peer, ZRPOS, "recovery ZRPOS")
	if zr2.Position() != badOff {
		t.Fatalf("recovery ZRPOS pos=%d, want %d (the append-only write offset)", zr2.Position(), badOff)
	}

	// Re-frame from the requested offset and finish.
	if err := peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, badOff)); err != nil {
		t.Fatalf("send re-framed ZDATA: %v", err)
	}
	for off := badOff; off < total; off += blk {
		end := off + blk
		endType := byte(ZCRCG)
		if end >= total {
			end = total
			endType = ZCRCE
		}
		if err := peer.sendSubpacket(content[off:end], endType); err != nil {
			t.Fatalf("resend subpacket at %d: %v", off, err)
		}
	}
	if err := peer.sendHexHeader(makePosHeader(ZEOF, total)); err != nil {
		t.Fatalf("send ZEOF: %v", err)
	}
	mustRecvType(t, peer, ZRINIT, "ZRINIT after ZEOF")
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatalf("send ZFIN: %v", err)
	}
	mustRecvType(t, peer, ZFIN, "receiver ZFIN")
	_ = peer.tw.writeRaw([]byte("OO"))
	_ = peer.tw.Flush()

	<-done
	if recvErr != nil {
		t.Fatalf("receiver returned error: %v", recvErr)
	}

	got, ok := recvHandler.receivedFiles["drain.bin"]
	if !ok {
		t.Fatal("drain.bin was not received")
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", got.Len(), total)
	}
	if n := rec.zrposCount(); n != 2 {
		t.Fatalf("receiver sent %d ZRPOS, want exactly 2 (initial + one quiesce cycle); "+
			"more means the old re-request-per-scan spam is still present", n)
	}
}

// TestReceiverDrainCapAborts pins the quiesce-drain bounded-failure path: a
// sender that streams continuously and never re-frames on ZRPOS must make the
// receiver hit the absolute drain cap and fail cleanly — never discard forever,
// never spin.
func TestReceiverDrainCapAborts(t *testing.T) {
	inCh := make(chan []byte, 4096) // peer -> receiver
	rr, rw := bufferedPipe(4096)    // receiver -> peer
	recvConn := &deadlineConn{in: inCh, out: rw}

	stop := make(chan struct{})
	cw := &corruptingWriter{w: &stoppableWriter{ch: inCh, stop: stop}, targetCount: 3}
	peerT := &pipeReadWriter{Reader: rr, Writer: cw}

	const (
		blk   = 256
		total = 1 << 20 // 1 MB nominal — far more than the peer ever delivers
	)
	content := make([]byte, 16*blk)
	for i := range content {
		content[i] = byte(i)
	}

	recvHandler := newTestHandler()
	receiver := NewSession(recvConn, recvHandler, &Config{
		MaxBlockSize:         blk,
		RecvTimeout:          time.Second,
		DataRecoveryQuietGap: 200 * time.Millisecond,
		DataRecoveryMaxBytes: 8192, // small cap → fires within a couple hundred ms
		DataRecoveryMaxWall:  5 * time.Second,
	})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: blk})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var recvErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		recvErr = receiver.Receive(ctx)
	}()

	// Continuously-streaming peer: offer the file, then after a corrupt packet
	// stream ZCRCG subpackets forever without ever honouring the reverse-channel
	// ZRPOS. Runs in its own goroutine and uses only goroutine-safe calls (no
	// t.Fatal); the first error, if any, is surfaced after teardown.
	var peerErr error
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		if _, peerErr = peer.recvHeader(); peerErr != nil { // ZRINIT
			return
		}
		fh := makeHeader(ZFILE)
		fh.SetZF0(ZCBIN)
		if peerErr = peer.sendBinHeader(fh); peerErr != nil {
			return
		}
		meta := marshalFileInfo(&FileOffer{Name: "flood.bin", Size: total}, 0, 0)
		if peerErr = peer.sendSubpacket(meta, ZCRCW); peerErr != nil {
			return
		}
		if _, peerErr = peer.recvHeader(); peerErr != nil { // ZRPOS(0)
			return
		}
		if peerErr = peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, 0)); peerErr != nil {
			return
		}
		for k := 0; ; k++ {
			off := (k % 16) * blk
			if err := peer.sendSubpacket(content[off:off+blk], ZCRCG); err != nil {
				return // stop closed (receiver aborted) or channel torn down
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	start := time.Now()
	<-done
	elapsed := time.Since(start)
	close(stop)
	<-peerDone

	if recvErr == nil {
		t.Fatal("receiver returned nil; a never-re-framing streamer must abort, not succeed")
	}
	if !errors.Is(recvErr, errDrainByteCap) {
		t.Fatalf("receiver error = %v, want the byte-cap drain abort (bounded clean failure)", recvErr)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("abort took %v; the drain cap must bound recovery, not spin", elapsed)
	}
	if err := recvHandler.completedFiles["flood.bin"]; err == nil {
		t.Fatal("flood.bin must be reported completed-with-error, not success")
	}
	_ = peerErr
}
