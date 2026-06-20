package zmodem

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestReceiverSkipWritesLowerResume pins the mid-stream resync fix: when the
// peer resumes a ZDATA frame BELOW the receiver's append-only write offset, the
// receiver must discard the overlapping [dataPos, fileOffset) bytes and write
// only the tail — converging on a byte-identical file instead of dead-locking in
// a re-ZRPOS loop. The resume offset is deliberately non-block-aligned.
func TestReceiverSkipWritesLowerResume(t *testing.T) {
	r1, w1 := bufferedPipe(256) // peer -> receiver
	r2, w2 := bufferedPipe(256) // receiver -> peer

	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}
	peerT := &pipeReadWriter{Reader: r2, Writer: w1}

	recvHandler := newTestHandler()
	receiver := NewSession(receiverT, recvHandler, &Config{MaxBlockSize: 4096})
	peer := NewSession(peerT, newTestHandler(), &Config{MaxBlockSize: 4096})

	const total = 4096
	content := make([]byte, total)
	for i := range content {
		content[i] = byte(i*7 + 3) // deterministic, non-trivial
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

	// Handshake.
	mustRecvType(t, peer, ZRINIT, "initial ZRINIT")
	fh := makeHeader(ZFILE)
	fh.SetZF0(ZCBIN)
	if err := peer.sendBinHeader(fh); err != nil {
		t.Fatalf("send ZFILE: %v", err)
	}
	meta := marshalFileInfo(&FileOffer{Name: "resume.bin", Size: total}, 0, 0)
	if err := peer.sendSubpacket(meta, ZCRCW); err != nil {
		t.Fatalf("send ZFILE metadata: %v", err)
	}
	zr := mustRecvType(t, peer, ZRPOS, "ZRPOS after ZFILE")
	if zr.Position() != 0 {
		t.Fatalf("initial ZRPOS pos=%d, want 0", zr.Position())
	}

	// Phase 1: deliver [0, F) and checkpoint with ZCRCW → the receiver's write
	// offset is now F (chosen non-block-aligned).
	const F = 1500
	if err := peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, 0)); err != nil {
		t.Fatalf("send phase-1 ZDATA: %v", err)
	}
	if err := peer.sendSubpacket(content[:F], ZCRCW); err != nil {
		t.Fatalf("send phase-1 subpacket: %v", err)
	}
	ack := mustRecvType(t, peer, ZACK, "ZACK after phase-1 ZCRCW")
	if ack.Position() != F {
		t.Fatalf("phase-1 ACK pos=%d, want %d", ack.Position(), F)
	}

	// Phase 2: resume BELOW the write offset (B < F, non-block-aligned) and
	// stream [B, total) in many small subpackets so the overlap [B, F) spans
	// far more subpackets than the data retry budget. The receiver must discard
	// the overlap, write only [F, total), and never emit another ZRPOS.
	const (
		B     = 700
		chunk = 64
	)
	if err := peer.sendBinHeaderWithZnulls(makePosHeader(ZDATA, B)); err != nil {
		t.Fatalf("send phase-2 ZDATA: %v", err)
	}
	for off := B; off < total; off += chunk {
		end := off + chunk
		endType := byte(ZCRCG)
		if end >= total {
			end = total
			endType = ZCRCE
		}
		if err := peer.sendSubpacket(content[off:end], endType); err != nil {
			t.Fatalf("send phase-2 subpacket at %d: %v", off, err)
		}
	}

	// EOF + teardown.
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
	w1.Close()

	if recvErr != nil {
		t.Fatalf("receiver returned error: %v", recvErr)
	}
	got, ok := recvHandler.receivedFiles["resume.bin"]
	if !ok {
		t.Fatal("resume.bin was not received")
	}
	if !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("content mismatch: got %d bytes, want %d (overlap duplicated or dropped?)",
			got.Len(), len(content))
	}
}

// TestReceiveDataSubpacketsResetsRetriesOnOverlap pins skip-write invariant 2:
// the data retry budget must reset on every VALID subpacket (good CRC), even one
// whose bytes fall wholly inside the discarded overlap and write nothing. A naive
// port that gates the reset on bytes-written would let the budget drain during a
// long below-offset catch-up and abort the very recovery it enables.
func TestReceiveDataSubpacketsResetsRetriesOnOverlap(t *testing.T) {
	// Encode a long run of valid CRC-16 subpackets followed by an empty ZCRCE.
	var enc bytes.Buffer
	encoder := NewSession(&pipeReadWriter{Reader: &bytes.Buffer{}, Writer: &enc}, newTestHandler(), &Config{})

	const (
		nSub   = dataRetryBudget * 2 // far more than the budget
		subLen = 32
	)
	for i := 0; i < nSub; i++ {
		if err := encoder.sendSubpacket(bytes.Repeat([]byte{'x'}, subLen), ZCRCG); err != nil {
			t.Fatalf("encode subpacket %d: %v", i, err)
		}
	}
	if err := encoder.sendSubpacket(nil, ZCRCE); err != nil {
		t.Fatalf("encode terminating ZCRCE: %v", err)
	}

	recv := NewSession(&pipeReadWriter{Reader: bytes.NewReader(enc.Bytes()), Writer: &bytes.Buffer{}},
		newTestHandler(), &Config{})

	var sink bytes.Buffer
	info := FileInfo{Name: "overlap.bin"}

	// Write offset sits ABOVE the entire incoming run, so every subpacket is a
	// pure duplicate: incomingPos starts at 0 and stays below offset throughout.
	offset := int64(nSub*subLen + 1000)
	incomingPos := int64(0)
	received := offset
	retries := dataRetryBudget // pre-charged right at the abort threshold

	err := recv.receiveDataSubpackets(context.Background(), &sink, &info,
		&offset, &incomingPos, &received, &retries)
	if err != nil {
		t.Fatalf("receiveDataSubpackets: %v", err)
	}

	if sink.Len() != 0 {
		t.Fatalf("wrote %d bytes for a wholly-overlapping run; want 0 "+
			"(append-only writer must not re-write the overlap)", sink.Len())
	}
	if retries != 0 {
		t.Fatalf("retries = %d after %d valid overlapping subpackets; want 0 "+
			"(retry budget must reset on every valid subpacket, not only on bytes written)",
			retries, nSub)
	}
	if want := int64(nSub * subLen); incomingPos != want {
		t.Fatalf("incomingPos = %d, want %d (must advance by full subpacket length even when discarded)",
			incomingPos, want)
	}
	if offset != int64(nSub*subLen+1000) {
		t.Fatalf("offset moved to %d; an append-only write offset must not change during a wholly-overlapping run", offset)
	}
}
