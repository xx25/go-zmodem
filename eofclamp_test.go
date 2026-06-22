package zmodem

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestReceiveDataSubpacketsClampsAtAnnouncedSize pins the EOF clamp at the unit
// level: when a sender (or a CRC-16 false-accept) streams MORE data than the
// file's announced size, the append-only write offset must stop at the
// announced size and the excess bytes must be dropped — never written past EOF.
// Without the clamp the offset overshoots, which later dead-locks the ZEOF.
func TestReceiveDataSubpacketsClampsAtAnnouncedSize(t *testing.T) {
	const (
		size    = 1000
		overrun = 200
		subLen  = 100
		total   = size + overrun
	)
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i*7 + 3)
	}

	// Encode total bytes as subLen-sized good-CRC subpackets ending in ZCRCE.
	var enc bytes.Buffer
	encoder := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &enc},
		newTestHandler(), &Config{MaxBlockSize: 4096})
	for off := 0; off < total; off += subLen {
		end := off + subLen
		et := byte(ZCRCG)
		if end >= total {
			end = total
			et = ZCRCE
		}
		if err := encoder.sendSubpacket(content[off:end], et); err != nil {
			t.Fatalf("encode subpacket at %d: %v", off, err)
		}
	}

	recv := NewSession(&pipeReadWriter{Reader: bytes.NewReader(enc.Bytes()), Writer: &bytes.Buffer{}},
		newTestHandler(), &Config{MaxBlockSize: 4096})
	var sink bytes.Buffer
	info := FileInfo{Name: "overrun.bin", Size: size}
	offset := int64(0)
	incomingPos := int64(0)
	received := int64(0)
	retries := 0

	if err := recv.receiveDataSubpackets(context.Background(), &sink, &info,
		&offset, &incomingPos, &received, &retries); err != nil {
		t.Fatalf("receiveDataSubpackets: %v", err)
	}

	if offset != size {
		t.Fatalf("write offset = %d, want %d (must clamp at the announced size)", offset, size)
	}
	if sink.Len() != size {
		t.Fatalf("wrote %d bytes, want %d (the %d overrun bytes past EOF must be dropped)",
			sink.Len(), size, overrun)
	}
	if !bytes.Equal(sink.Bytes(), content[:size]) {
		t.Fatal("clamped content mismatch: the written file must be the first `size` bytes")
	}
	// The incoming-stream cursor still advances over every consumed byte.
	if incomingPos != total {
		t.Fatalf("incomingPos = %d, want %d (the stream cursor counts every consumed byte)", incomingPos, total)
	}
}

// TestReceiverClampsOverrunAtEOFWithoutDeadlock is the end-to-end 8912
// regression: a sender streams `overrun` bytes past the announced file size in
// one ZDATA frame (modelling a CRC-16 residue false-accept) and then sends the
// legitimate ZEOF at the true size. Before the clamp the receiver's offset
// overshot the ZEOF, it rejected the ZEOF as an offset mismatch and spun ZRPOS
// for bytes the sender never had — dead-locking until carrier loss. With the
// clamp the offset lands exactly on the size, the ZEOF matches, and the file
// completes cleanly with exactly `size` bytes. The "ZRINIT after ZEOF" recv is
// the deadlock detector: a pre-fix receiver would answer ZRPOS there.
func TestReceiverClampsOverrunAtEOFWithoutDeadlock(t *testing.T) {
	r1, w1 := bufferedPipe(256) // peer -> receiver
	r2, w2 := bufferedPipe(256) // receiver -> peer
	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}
	peerT := &pipeReadWriter{Reader: r2, Writer: w1}

	recvHandler := newTestHandler()
	receiver := NewSession(receiverT, recvHandler, &Config{MaxBlockSize: 4096})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: 4096})

	const (
		size    = 2000
		overrun = 300
	)
	content := make([]byte, size+overrun)
	for i := range content {
		content[i] = byte(i*5 + 1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var recvErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w2.Close()
		recvErr = receiver.Receive(ctx)
	}()

	mustRecvType(t, peer, ZRINIT, "initial ZRINIT")
	fh := makeHeader(ZFILE)
	fh.SetZF0(ZCBIN)
	if err := peer.sendBinHeader(fh); err != nil {
		t.Fatalf("send ZFILE: %v", err)
	}
	meta := marshalFileInfo(&FileOffer{Name: "overrun.bin", Size: size}, 0, 0)
	if err := peer.sendSubpacket(meta, ZCRCW); err != nil {
		t.Fatalf("send ZFILE metadata: %v", err)
	}
	zr := mustRecvType(t, peer, ZRPOS, "ZRPOS after ZFILE")
	if zr.Position() != 0 {
		t.Fatalf("ZRPOS pos=%d, want 0", zr.Position())
	}

	// Stream size+overrun bytes in one ZDATA frame.
	if err := peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, 0)); err != nil {
		t.Fatalf("send ZDATA: %v", err)
	}
	const chunk = 256
	for off := 0; off < len(content); off += chunk {
		end := off + chunk
		et := byte(ZCRCG)
		if end >= len(content) {
			end = len(content)
			et = ZCRCE
		}
		if err := peer.sendSubpacket(content[off:end], et); err != nil {
			t.Fatalf("send subpacket at %d: %v", off, err)
		}
	}
	// The sender's true EOF is at the announced size.
	if err := peer.sendHexHeader(makePosHeader(ZEOF, size)); err != nil {
		t.Fatalf("send ZEOF: %v", err)
	}

	// The deadlock detector: a pre-clamp receiver overshoots and answers ZRPOS
	// here instead of advancing to the next file with ZRINIT.
	mustRecvType(t, peer, ZRINIT, "ZRINIT after ZEOF (no deadlock)")
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
	got, ok := recvHandler.receivedFiles["overrun.bin"]
	if !ok {
		t.Fatal("overrun.bin was not received")
	}
	if got.Len() != size {
		t.Fatalf("received %d bytes, want %d (the %d overrun bytes past EOF must be clamped)",
			got.Len(), size, overrun)
	}
	if !bytes.Equal(got.Bytes(), content[:size]) {
		t.Fatal("clamped content mismatch")
	}
	if e := recvHandler.completedFiles["overrun.bin"]; e != nil {
		t.Fatalf("file completed with error: %v", e)
	}
}

// TestReceiverFailsFastOnEOFBelowOffsetWhenSizeUnknown covers the size-unknown
// path where the in-loop clamp cannot bound the offset: the sender omits the
// size (Size=0), a false-accept over-advances the offset, and the sender then
// sends a ZEOF BELOW the offset. The receiver must abort fast with
// errOverwritePastEOF rather than spin ZRPOS until carrier loss.
func TestReceiverFailsFastOnEOFBelowOffsetWhenSizeUnknown(t *testing.T) {
	r1, w1 := bufferedPipe(256)
	r2, w2 := bufferedPipe(256)
	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}
	peerT := &pipeReadWriter{Reader: r2, Writer: w1}

	recvHandler := newTestHandler()
	receiver := NewSession(receiverT, recvHandler, &Config{MaxBlockSize: 4096})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: 4096})

	const (
		eof   = 1000 // the sender's declared EOF
		total = 1200 // bytes actually streamed (offset overshoots eof)
	)
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i*3 + 9)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var recvErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer w2.Close()
		recvErr = receiver.Receive(ctx)
	}()

	mustRecvType(t, peer, ZRINIT, "initial ZRINIT")
	fh := makeHeader(ZFILE)
	fh.SetZF0(ZCBIN)
	if err := peer.sendBinHeader(fh); err != nil {
		t.Fatalf("send ZFILE: %v", err)
	}
	// Size 0 = unknown → the clamp is disabled, so the offset can overshoot.
	meta := marshalFileInfo(&FileOffer{Name: "nosize.bin", Size: 0}, 0, 0)
	if err := peer.sendSubpacket(meta, ZCRCW); err != nil {
		t.Fatalf("send ZFILE metadata: %v", err)
	}
	mustRecvType(t, peer, ZRPOS, "ZRPOS after ZFILE")

	if err := peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, 0)); err != nil {
		t.Fatalf("send ZDATA: %v", err)
	}
	const chunk = 256
	for off := 0; off < total; off += chunk {
		end := off + chunk
		et := byte(ZCRCG)
		if end >= total {
			end = total
			et = ZCRCE
		}
		if err := peer.sendSubpacket(content[off:end], et); err != nil {
			t.Fatalf("send subpacket at %d: %v", off, err)
		}
	}
	// ZEOF below the receiver's (overshot) write offset.
	if err := peer.sendHexHeader(makePosHeader(ZEOF, eof)); err != nil {
		t.Fatalf("send ZEOF: %v", err)
	}

	<-done
	w1.Close()

	if !errors.Is(recvErr, errOverwritePastEOF) {
		t.Fatalf("recvErr = %v, want errOverwritePastEOF (fail fast, no ZRPOS deadlock)", recvErr)
	}
	if e := recvHandler.completedFiles["nosize.bin"]; !errors.Is(e, errOverwritePastEOF) {
		t.Fatalf("FileCompleted err = %v, want errOverwritePastEOF", e)
	}
}
