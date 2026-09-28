package zmodem

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// peerRefuseFile reads a ZFILE + metadata subpacket and answers it with hdr.
func peerRefuseFile(t *testing.T, s *Session, answer byte) FileInfo {
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
	if err := s.sendHexHeader(makeHeader(answer)); err != nil {
		t.Fatalf("send %s: %v", frameTypeName(answer), err)
	}
	return info
}

// TestSenderZFERRSkipsOneFile pins that a receiver answering ZFILE with ZFERR
// (FrontDoor, on a name DOS cannot create) refuses that one file, not the
// batch: the sender reports it as a skip and goes on to the next file.
func TestSenderZFERRSkipsOneFile(t *testing.T) {
	r1, w1 := bufferedPipe(256)
	r2, w2 := bufferedPipe(256)
	senderT := &pipeReadWriter{Reader: r2, Writer: w1}
	peerT := &pipeReadWriter{Reader: r1, Writer: w2}

	bad, good := []byte("unstorable"), []byte("the netmail packet")
	h := newTestHandler()
	h.filesToSend = []*FileOffer{
		{Name: "файл.txt", Size: int64(len(bad)), Reader: bytes.NewReader(bad)},
		{Name: "mail.pkt", Size: int64(len(good)), Reader: bytes.NewReader(good)},
	}
	sender := NewSession(senderT, h, &Config{MaxBlockSize: 1024})
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

	mustRecvType(t, peer, ZRQINIT, "ZRQINIT")
	if err := peer.sendZRINIT(); err != nil {
		t.Fatal(err)
	}
	if info := peerRefuseFile(t, peer, ZFERR); info.Name != "файл.txt" {
		t.Fatalf("first offer %q", info.Name)
	}
	info, data := peerReceiveOneFile(t, peer)
	if info.Name != "mail.pkt" || !bytes.Equal(data, good) {
		t.Fatalf("second file %q = %q", info.Name, data)
	}
	mustRecvType(t, peer, ZFIN, "ZFIN")
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatal(err)
	}
	<-done
	w2.Close()

	if sendErr != nil {
		t.Fatalf("sender: %v", sendErr)
	}
	if err := h.completedFiles["файл.txt"]; !errors.Is(err, ErrSkip) {
		t.Fatalf("refused file completed with %v, want ErrSkip", err)
	}
	if err := h.completedFiles["mail.pkt"]; err != nil {
		t.Fatalf("second file completed with %v", err)
	}
}

// TestSenderIgnoresLateZACKAfterZEOF pins that ZACKs for earlier ZCRCQ frames,
// arriving after our ZEOF (a soft modem buffering ahead of a slow UART, as in
// front of Brake!), do not end the session: the sender waits for ZRINIT.
func TestSenderIgnoresLateZACKAfterZEOF(t *testing.T) {
	r1, w1 := bufferedPipe(256)
	r2, w2 := bufferedPipe(256)
	senderT := &pipeReadWriter{Reader: r2, Writer: w1}
	peerT := &pipeReadWriter{Reader: r1, Writer: w2}

	content := []byte("a file acknowledged late")
	h := newTestHandler()
	h.filesToSend = []*FileOffer{{Name: "late.bin", Size: int64(len(content)), Reader: bytes.NewReader(content)}}
	sender := NewSession(senderT, h, &Config{MaxBlockSize: 1024})
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

	mustRecvType(t, peer, ZRQINIT, "ZRQINIT")
	if err := peer.sendZRINIT(); err != nil {
		t.Fatal(err)
	}
	mustRecvType(t, peer, ZFILE, "ZFILE")
	if _, _, err := peer.recvSubpacket(2048); err != nil {
		t.Fatal(err)
	}
	if err := peer.sendHexHeader(makePosHeader(ZRPOS, 0)); err != nil {
		t.Fatal(err)
	}
	mustRecvType(t, peer, ZDATA, "ZDATA")
	for {
		_, end, err := peer.recvSubpacket(2048)
		if err != nil {
			t.Fatal(err)
		}
		if end == ZCRCE {
			break
		}
	}
	mustRecvType(t, peer, ZEOF, "ZEOF")
	for i := 0; i < 3; i++ {
		if err := peer.sendHexHeader(makePosHeader(ZACK, int64(i*8))); err != nil {
			t.Fatal(err)
		}
	}
	if err := peer.sendZRINIT(); err != nil {
		t.Fatal(err)
	}
	mustRecvType(t, peer, ZFIN, "ZFIN")
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatal(err)
	}
	<-done
	w2.Close()

	if sendErr != nil {
		t.Fatalf("sender: %v", sendErr)
	}
	if err, ok := h.completedFiles["late.bin"]; !ok || err != nil {
		t.Fatalf("late.bin completed=%v err=%v", ok, err)
	}
}

// TestSenderResendsZFINOnSilence pins the other half of the ZFIN rule: when
// the receiver says NOTHING (D'Bridge ignores the first ZFIN of an empty
// batch), the sender re-sends ZFIN after a timeout, as sz does, and finishes
// with OO once it is answered. A header that crossed the ZFIN is a different
// case (TestSenderWaitsForZFINAfterStrayZRINIT): that one is never re-sent.
func TestSenderResendsZFINOnSilence(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()

	sender := NewSession(c1, newTestHandler(), &Config{MaxBlockSize: 1024, RecvTimeout: 200 * time.Millisecond})
	peer := NewSession(c2, newTestHandler(), &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sendErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer c1.Close()
		sendErr = sender.Send(ctx)
	}()

	mustRecvType(t, peer, ZRQINIT, "ZRQINIT")
	if err := peer.sendZRINIT(); err != nil {
		t.Fatal(err)
	}
	mustRecvType(t, peer, ZFIN, "first ZFIN")
	// Ignore it, as D'Bridge does. The sender must ask again.
	mustRecvType(t, peer, ZFIN, "re-sent ZFIN")
	if err := peer.sendHexHeader(makeHeader(ZFIN)); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(c2)
	<-done

	if sendErr != nil {
		t.Fatalf("sender: %v", sendErr)
	}
	if !bytes.Contains(rest, []byte("OO")) {
		t.Fatalf("no OO after the answered ZFIN; trailing bytes %q", rest)
	}
}
