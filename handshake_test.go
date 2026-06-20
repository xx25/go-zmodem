package zmodem

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// --- scripted-peer helpers ---------------------------------------------------
//
// These tests drive one real Session (sender or receiver) via Send()/Receive()
// in a goroutine and hand-script the opposite peer using the same low-level
// frame helpers, so the exact frame sequence — including the abnormal headers
// these fixes target — is fully under the test's control.

// mustRecvType reads one header from the scripted peer and fails the test if it
// is not the wanted type. A ZNAK is called out explicitly because the file-wait
// fix is precisely "never answer with ZNAK here".
func mustRecvType(t *testing.T, s *Session, want byte, what string) Header {
	t.Helper()
	hdr, err := s.recvHeader()
	if err != nil {
		t.Fatalf("%s: recvHeader: %v", what, err)
	}
	if hdr.Type == ZNAK && want != ZNAK {
		t.Fatalf("%s: peer sent ZNAK (the deadlock bug); want %s", what, frameTypeName(want))
	}
	if hdr.Type != want {
		t.Fatalf("%s: got %s, want %s", what, frameTypeName(hdr.Type), frameTypeName(want))
	}
	return hdr
}

// corruptHexHeader builds a complete hex frame whose CRC is deliberately wrong,
// to force a recvHeader CRC failure on the receiving side.
func corruptHexHeader(frameType byte) []byte {
	var payload [5]byte
	payload[0] = frameType
	crc := crc16Calc(payload[:]) ^ 0xffff // flip every bit so verification fails

	const hexDigits = "0123456789abcdef"
	out := []byte{ZPAD, ZPAD, ZDLE, ZHEX}
	appendHex := func(b byte) { out = append(out, hexDigits[b>>4], hexDigits[b&0x0f]) }
	for _, p := range payload {
		appendHex(p)
	}
	appendHex(byte(crc >> 8))
	appendHex(byte(crc & 0xff))
	out = append(out, 0x0d, 0x0a)
	return out
}

// peerSendOneFile scripts the send side of a single small file to a Session that
// is running Receive(): ZFILE+metadata, then (after the receiver's ZRPOS) a
// single-subpacket ZDATA frame and ZEOF, and finally consumes the receiver's
// next-file ZRINIT.
func peerSendOneFile(t *testing.T, s *Session, name string, content []byte) {
	t.Helper()

	fh := makeHeader(ZFILE)
	fh.SetZF0(ZCBIN)
	if err := s.sendBinHeader(fh); err != nil {
		t.Fatalf("send ZFILE: %v", err)
	}
	meta := marshalFileInfo(&FileOffer{Name: name, Size: int64(len(content))}, 0, 0)
	if err := s.sendSubpacket(meta, ZCRCW); err != nil {
		t.Fatalf("send ZFILE metadata: %v", err)
	}

	zr := mustRecvType(t, s, ZRPOS, "ZRPOS for "+name)
	if zr.Position() != 0 {
		t.Fatalf("ZRPOS for %s: pos=%d, want 0", name, zr.Position())
	}

	if err := s.sendBinHeaderWithZnulls(makePosHeader(ZDATA, 0)); err != nil {
		t.Fatalf("send ZDATA: %v", err)
	}
	if err := s.sendSubpacket(content, ZCRCE); err != nil {
		t.Fatalf("send data subpacket: %v", err)
	}
	if err := s.sendHexHeader(makePosHeader(ZEOF, int64(len(content)))); err != nil {
		t.Fatalf("send ZEOF: %v", err)
	}

	mustRecvType(t, s, ZRINIT, "ZRINIT after ZEOF for "+name)
}

// peerReceiveOneFile scripts the receive side of a single small file from a
// Session that is running Send(): consume ZFILE+metadata, answer ZRPOS(0), read
// the ZDATA frame's subpackets up to ZCRCE, consume ZEOF, then answer ZRINIT.
// Returns the parsed FileInfo and the reassembled file bytes.
func peerReceiveOneFile(t *testing.T, s *Session) (FileInfo, []byte) {
	t.Helper()

	hdr := mustRecvType(t, s, ZFILE, "ZFILE")
	if hdr.Encoding == ZBIN32 {
		s.useCRC32 = true
	}
	meta, _, err := s.recvSubpacket(2048)
	if err != nil {
		t.Fatalf("read ZFILE metadata: %v", err)
	}
	info, err := parseFileInfo(meta)
	if err != nil {
		t.Fatalf("parse file info: %v", err)
	}

	if err := s.sendHexHeader(makePosHeader(ZRPOS, 0)); err != nil {
		t.Fatalf("send ZRPOS: %v", err)
	}

	zd := mustRecvType(t, s, ZDATA, "ZDATA")
	if zd.Encoding == ZBIN32 {
		s.useCRC32 = true
	}

	var data []byte
	for {
		sub, endType, err := s.recvSubpacket(s.cfg.MaxBlockSize + 256)
		if err != nil {
			t.Fatalf("read data subpacket: %v", err)
		}
		data = append(data, sub...)
		if endType == ZCRCG {
			continue
		}
		if endType != ZCRCE {
			t.Fatalf("unexpected subpacket end type 0x%02x (test file should fit one ZCRCG+ZCRCE)", endType)
		}
		break
	}

	mustRecvType(t, s, ZEOF, "ZEOF")

	if err := s.sendZRINIT(); err != nil {
		t.Fatalf("send next-file ZRINIT: %v", err)
	}
	return info, data
}

// recordWriter records every byte written through it (for post-hoc frame
// counting) while forwarding to the underlying writer.
type recordWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
}

func (r *recordWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf.Write(p)
	r.mu.Unlock()
	return r.w.Write(p)
}

func (r *recordWriter) snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf.Bytes()...)
}

// TestReceiverFileWaitResendsZRINIT pins the file-wait fix: a recvHeader failure
// while waiting for the first ZFILE must be answered with a ZRINIT resend, never
// a ZNAK. Peers that mirror ZNAK (e.g. XPGE) otherwise deadlock the handshake.
// Here the failure is injected as a corrupt header; the receiver must re-prompt
// with ZRINIT and then accept a real, late ZFILE.
func TestReceiverFileWaitResendsZRINIT(t *testing.T) {
	r1, w1 := bufferedPipe(256) // peer -> receiver
	r2, w2 := bufferedPipe(256) // receiver -> peer

	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}
	peerT := &pipeReadWriter{Reader: r2, Writer: w1}

	recvHandler := newTestHandler()
	receiver := NewSession(receiverT, recvHandler, &Config{MaxBlockSize: 1024})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var recvErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w2.Close()
		recvErr = receiver.Receive(ctx)
	}()

	content := []byte("a file delivered after a file-wait stumble")

	// 1. The receiver's initial ZRINIT.
	mustRecvType(t, peer, ZRINIT, "initial ZRINIT")

	// 2. Inject a corrupt header — a recvHeader failure in the file-wait state.
	if err := peer.tw.writeRaw(corruptHexHeader(ZFILE)); err != nil {
		t.Fatalf("write corrupt header: %v", err)
	}
	if err := peer.tw.Flush(); err != nil {
		t.Fatalf("flush corrupt header: %v", err)
	}

	// 3. The fix: re-prompt with ZRINIT, not ZNAK.
	mustRecvType(t, peer, ZRINIT, "ZRINIT resend after corrupt header")

	// 4. Deliver a real file; the receiver must accept the late ZFILE.
	peerSendOneFile(t, peer, "late.txt", content)

	// 5. End the batch.
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatalf("send ZFIN: %v", err)
	}
	mustRecvType(t, peer, ZFIN, "receiver ZFIN")
	_ = peer.tw.writeRaw([]byte("OO"))
	_ = peer.tw.Flush()

	<-done
	w1.Close()

	if recvErr != nil {
		t.Fatalf("receiver returned error: %v", recvErr)
	}
	got, ok := recvHandler.receivedFiles["late.txt"]
	if !ok {
		t.Fatal("late.txt was not received")
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("content mismatch: got %q, want %q", got.Bytes(), content)
	}
}

// TestSenderToleratesTurnaroundZFIN pins the turnaround fix: when the peer
// answers the sender's ZRQINIT with a spurious ZFIN (a WaZOO turnaround
// artifact, e.g. T-Mail), the sender must tolerate it, resend ZRQINIT, and go on
// to deliver the batch — rather than aborting "expected ZRINIT, got ZFIN". It
// also pins that the rz\r auto-download preamble is emitted exactly once across
// the retried ZRQINITs.
func TestSenderToleratesTurnaroundZFIN(t *testing.T) {
	r1, rawW1 := bufferedPipe(256) // sender -> peer
	r2, w2 := bufferedPipe(256)    // peer -> sender

	// Record everything the sender emits to count rz\r / ZRQINIT occurrences.
	rec := &recordWriter{w: rawW1}

	senderT := &pipeReadWriter{Reader: r2, Writer: rec}
	peerT := &pipeReadWriter{Reader: r1, Writer: w2}

	content := []byte("outbound bundle for a turnaround peer")
	sendHandler := newTestHandler()
	sendHandler.filesToSend = []*FileOffer{
		{Name: "outbound.pkt", Size: int64(len(content)), Reader: bytes.NewReader(content)},
	}
	sender := NewSession(senderT, sendHandler, &Config{MaxBlockSize: 1024})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rawW1.Close()
		sendErr = sender.Send(ctx)
	}()

	// 1. The sender's first ZRQINIT (rz\r preamble is skipped as garbage).
	mustRecvType(t, peer, ZRQINIT, "first ZRQINIT")

	// 2. Answer with a spurious turnaround ZFIN instead of ZRINIT.
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatalf("send turnaround ZFIN: %v", err)
	}

	// 3. The fix: the sender tolerates it and resends ZRQINIT.
	mustRecvType(t, peer, ZRQINIT, "ZRQINIT resend after turnaround ZFIN")

	// 4. Now answer ZRINIT; the sender proceeds to deliver the file.
	if err := peer.sendZRINIT(); err != nil {
		t.Fatalf("send ZRINIT: %v", err)
	}
	info, data := peerReceiveOneFile(t, peer)
	if info.Name != "outbound.pkt" {
		t.Fatalf("received file name %q, want outbound.pkt", info.Name)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("content mismatch: got %q, want %q", data, content)
	}

	// 5. Session teardown: sender ends with ZFIN, we answer ZFIN, it sends OO.
	mustRecvType(t, peer, ZFIN, "sender ZFIN")
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatalf("send teardown ZFIN: %v", err)
	}

	<-done
	w2.Close()

	if sendErr != nil {
		t.Fatalf("sender returned error: %v", sendErr)
	}

	// The rz\r preamble must have fired exactly once even though ZRQINIT was
	// sent twice (initial + after the tolerated ZFIN).
	out := rec.snapshot()
	if n := bytes.Count(out, AutoDownloadString); n != 1 {
		t.Fatalf("rz\\r auto-download preamble sent %d times, want exactly 1", n)
	}
	zrqinit := []byte{ZPAD, ZPAD, ZDLE, ZHEX, '0', '0'} // hex ZRQINIT (type 0x00)
	if n := bytes.Count(out, zrqinit); n != 2 {
		t.Fatalf("ZRQINIT sent %d times, want 2 (initial + one resend)", n)
	}
}

// TestSenderTurnaroundZFINBounded pins that the ZFIN tolerance is bounded by
// maxSkipFin (a counter separate from the read-retry budget): a peer that
// answers ZFIN forever makes the sender fail cleanly rather than loop.
func TestSenderTurnaroundZFINBounded(t *testing.T) {
	r1, w1 := bufferedPipe(256) // sender -> peer
	r2, w2 := bufferedPipe(256) // peer -> sender

	senderT := &pipeReadWriter{Reader: r2, Writer: w1}
	peerT := &pipeReadWriter{Reader: r1, Writer: w2}

	content := []byte("never delivered")
	sendHandler := newTestHandler()
	sendHandler.filesToSend = []*FileOffer{
		{Name: "stuck.pkt", Size: int64(len(content)), Reader: bytes.NewReader(content)},
	}
	sender := NewSession(senderT, sendHandler, &Config{MaxBlockSize: 1024})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w1.Close()
		sendErr = sender.Send(ctx)
	}()

	// Read the initial ZRQINIT, then answer ZFIN to every resent ZRQINIT. The
	// sender tolerates maxSkipFin of them and errors on the next one.
	mustRecvType(t, peer, ZRQINIT, "initial ZRQINIT")
	for i := 0; i < maxSkipFin; i++ {
		if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
			t.Fatalf("send ZFIN #%d: %v", i+1, err)
		}
		mustRecvType(t, peer, ZRQINIT, "ZRQINIT resend")
	}
	// One more ZFIN exceeds the bound.
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatalf("send final ZFIN: %v", err)
	}

	<-done
	w2.Close()

	if sendErr == nil {
		t.Fatal("sender accepted unbounded turnaround ZFINs; want a clean error after maxSkipFin")
	}
}
