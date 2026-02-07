package zmodem

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chanReader reads byte slices from a channel. When the channel is closed,
// Read returns io.EOF. This provides non-blocking writes (up to channel buffer
// capacity) which prevents deadlock when both sides write before reading.
type chanReader struct {
	ch  chan []byte
	buf []byte
}

func (cr *chanReader) Read(p []byte) (int, error) {
	if len(cr.buf) > 0 {
		n := copy(p, cr.buf)
		cr.buf = cr.buf[n:]
		return n, nil
	}
	data, ok := <-cr.ch
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, data)
	if n < len(data) {
		cr.buf = data[n:]
	}
	return n, nil
}

// chanWriter writes byte slice copies to a channel.
type chanWriter struct {
	ch chan []byte
}

func (cw *chanWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	cw.ch <- buf
	return len(p), nil
}

func (cw *chanWriter) Close() error {
	close(cw.ch)
	return nil
}

// bufferedPipe creates a unidirectional pipe with channel-based buffering.
// Unlike io.Pipe, writes are non-blocking up to bufSize pending messages.
func bufferedPipe(bufSize int) (*chanReader, *chanWriter) {
	ch := make(chan []byte, bufSize)
	return &chanReader{ch: ch}, &chanWriter{ch: ch}
}

// pipeReadWriter combines an io.Reader and io.Writer into an io.ReadWriter.
type pipeReadWriter struct {
	io.Reader
	io.Writer
}

// testFileHandler implements FileHandler for testing.
type testFileHandler struct {
	mu             sync.Mutex
	filesToSend    []*FileOffer
	sendIdx        int
	receivedFiles  map[string]*bytes.Buffer
	completedFiles map[string]error
	progress       map[string]int64
	acceptOffset   int64
	skipFiles      map[string]bool
}

func newTestHandler() *testFileHandler {
	return &testFileHandler{
		receivedFiles:  make(map[string]*bytes.Buffer),
		completedFiles: make(map[string]error),
		progress:       make(map[string]int64),
		skipFiles:      make(map[string]bool),
	}
}

func (h *testFileHandler) NextFile() *FileOffer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sendIdx >= len(h.filesToSend) {
		return nil
	}
	f := h.filesToSend[h.sendIdx]
	h.sendIdx++
	return f
}

type nopWriteCloser struct {
	*bytes.Buffer
}

func (nwc *nopWriteCloser) Close() error { return nil }

func (h *testFileHandler) AcceptFile(info FileInfo) (io.WriteCloser, int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.skipFiles[info.Name] {
		return nil, 0, ErrSkip
	}

	buf := &bytes.Buffer{}
	h.receivedFiles[info.Name] = buf
	return &nopWriteCloser{buf}, h.acceptOffset, nil
}

func (h *testFileHandler) FileProgress(info FileInfo, bytesTransferred int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.progress[info.Name] = bytesTransferred
}

func (h *testFileHandler) FileCompleted(info FileInfo, bytesTransferred int64, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completedFiles[info.Name] = err
}

// newTestTransports creates a pair of buffered transports for sender and receiver.
func newTestTransports() (senderT, receiverT io.ReadWriter, senderClose, receiverClose func()) {
	// Channel 1: sender writes -> receiver reads
	r1, w1 := bufferedPipe(256)
	// Channel 2: receiver writes -> sender reads
	r2, w2 := bufferedPipe(256)

	senderT = &pipeReadWriter{Reader: r2, Writer: w1}
	receiverT = &pipeReadWriter{Reader: r1, Writer: w2}
	senderClose = func() { w1.Close() }
	receiverClose = func() { w2.Close() }
	return
}

func TestLoopbackSingleFile(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	// Test data
	testContent := []byte("Hello, ZMODEM loopback test! This is a test file.")

	// Sender handler
	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:    "test.txt",
			Size:    int64(len(testContent)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(testContent),
		},
	}

	// Receiver handler
	receiverHandler := newTestHandler()

	// Create sessions
	senderCfg := &Config{MaxBlockSize: 1024}
	receiverCfg := &Config{MaxBlockSize: 1024}

	sender := NewSession(senderTransport, senderHandler, senderCfg)
	receiver := NewSession(receiverTransport, receiverHandler, receiverCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run sender and receiver concurrently
	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	// Verify received file
	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	received, ok := receiverHandler.receivedFiles["test.txt"]
	if !ok {
		t.Fatal("file 'test.txt' not received")
	}

	if !bytes.Equal(received.Bytes(), testContent) {
		t.Errorf("received content mismatch: got %d bytes, want %d bytes", received.Len(), len(testContent))
	}

	// Check completion
	if err, ok := receiverHandler.completedFiles["test.txt"]; !ok {
		t.Error("file not marked as completed")
	} else if err != nil {
		t.Errorf("file completed with error: %v", err)
	}
}

func TestLoopbackBatchFiles(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	// Multiple files
	files := []struct {
		name    string
		content []byte
	}{
		{"file1.txt", []byte("First file content")},
		{"file2.bin", make([]byte, 4096)},
		{"file3.dat", []byte("Third file")},
	}

	// Fill file2 with random data
	rand.Read(files[1].content)

	senderHandler := newTestHandler()
	for _, f := range files {
		senderHandler.filesToSend = append(senderHandler.filesToSend, &FileOffer{
			Name:   f.name,
			Size:   int64(len(f.content)),
			Mode:   0644,
			Reader: bytes.NewReader(f.content),
		})
	}

	receiverHandler := newTestHandler()

	sender := NewSession(senderTransport, senderHandler, &Config{MaxBlockSize: 512})
	receiver := NewSession(receiverTransport, receiverHandler, &Config{MaxBlockSize: 512})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	for _, f := range files {
		received, ok := receiverHandler.receivedFiles[f.name]
		if !ok {
			t.Errorf("file %q not received", f.name)
			continue
		}
		if !bytes.Equal(received.Bytes(), f.content) {
			t.Errorf("file %q content mismatch: got %d bytes, want %d bytes",
				f.name, received.Len(), len(f.content))
		}
	}
}

func TestLoopbackSkipFile(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "skip_me.txt",
			Size:   100,
			Reader: bytes.NewReader(make([]byte, 100)),
		},
		{
			Name:   "keep_me.txt",
			Size:   50,
			Reader: bytes.NewReader([]byte("keep this file content - it should be received")),
		},
	}

	receiverHandler := newTestHandler()
	receiverHandler.skipFiles["skip_me.txt"] = true

	sender := NewSession(senderTransport, senderHandler, nil)
	receiver := NewSession(receiverTransport, receiverHandler, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	// skip_me.txt should not be in receivedFiles
	if _, ok := receiverHandler.receivedFiles["skip_me.txt"]; ok {
		t.Error("skip_me.txt should not have been received")
	}

	// keep_me.txt should be received
	if _, ok := receiverHandler.receivedFiles["keep_me.txt"]; !ok {
		t.Error("keep_me.txt should have been received")
	}
}

func TestLoopbackEmptyFile(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "empty.txt",
			Size:   0,
			Reader: bytes.NewReader([]byte{}),
		},
	}

	receiverHandler := newTestHandler()

	sender := NewSession(senderTransport, senderHandler, nil)
	receiver := NewSession(receiverTransport, receiverHandler, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	received, ok := receiverHandler.receivedFiles["empty.txt"]
	if !ok {
		t.Fatal("empty.txt not received")
	}
	if received.Len() != 0 {
		t.Errorf("empty.txt should have 0 bytes, got %d", received.Len())
	}
}

func TestLoopbackLargeFile(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	// 64KB file with random data
	largeContent := make([]byte, 65536)
	rand.Read(largeContent)

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "large.bin",
			Size:   int64(len(largeContent)),
			Reader: bytes.NewReader(largeContent),
		},
	}

	receiverHandler := newTestHandler()

	sender := NewSession(senderTransport, senderHandler, &Config{MaxBlockSize: 1024})
	receiver := NewSession(receiverTransport, receiverHandler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	received, ok := receiverHandler.receivedFiles["large.bin"]
	if !ok {
		t.Fatal("large.bin not received")
	}
	if !bytes.Equal(received.Bytes(), largeContent) {
		t.Errorf("large.bin content mismatch: got %d bytes, want %d bytes",
			received.Len(), len(largeContent))
	}
}

func TestLoopbackCRC32(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	testContent := []byte("Testing CRC-32 mode transfer!")

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "crc32test.txt",
			Size:   int64(len(testContent)),
			Reader: bytes.NewReader(testContent),
		},
	}

	receiverHandler := newTestHandler()

	sender := NewSession(senderTransport, senderHandler, &Config{Use32BitCRC: true})
	receiver := NewSession(receiverTransport, receiverHandler, &Config{Use32BitCRC: true})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	received, ok := receiverHandler.receivedFiles["crc32test.txt"]
	if !ok {
		t.Fatal("crc32test.txt not received")
	}
	if !bytes.Equal(received.Bytes(), testContent) {
		t.Errorf("content mismatch")
	}
}

// corruptingWriter wraps an io.Writer and corrupts the CRC of the Nth
// ZCRCG subpacket (identified by the ZDLE+ZCRCG byte pair in the stream).
type corruptingWriter struct {
	w            io.Writer
	targetCount  int     // which subpacket to corrupt (1-based)
	currentCount int32   // atomic counter for ZCRCG sequences seen
	prev         byte    // previous byte for ZDLE detection
	corrupted    atomic.Bool
}

func (cw *corruptingWriter) Write(p []byte) (int, error) {
	if cw.corrupted.Load() {
		return cw.w.Write(p)
	}

	// Scan for ZDLE+ZCRCG pairs; corrupt the next 2 bytes (CRC) after the target
	buf := make([]byte, len(p))
	copy(buf, p)

	for i := 0; i < len(buf); i++ {
		if cw.prev == ZDLE && buf[i] == ZCRCG {
			cw.currentCount++
			if int(cw.currentCount) == cw.targetCount {
				// Corrupt the CRC bytes that follow this ZCRCG.
				// They may be in this write or a subsequent one.
				// Corrupt what we can in this buffer.
				for j := i + 1; j < len(buf) && j <= i+4; j++ {
					buf[j] ^= 0xFF
				}
				cw.corrupted.Store(true)
			}
		}
		cw.prev = buf[i]
	}

	return cw.w.Write(buf)
}

func TestLoopbackMidStreamZRPOS(t *testing.T) {
	// Create channel-based transports
	// Channel 1: sender writes -> (corrupt) -> receiver reads
	r1, w1 := bufferedPipe(256)
	// Channel 2: receiver writes -> sender reads
	r2, w2 := bufferedPipe(256)

	// Wrap w1 with a corrupting writer that corrupts the 3rd ZCRCG subpacket
	cw := &corruptingWriter{w: w1, targetCount: 3}

	senderT := &pipeReadWriter{Reader: r2, Writer: cw}
	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}

	// 16KB file to ensure enough subpackets for corruption to trigger
	testContent := make([]byte, 16384)
	rand.Read(testContent)

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "corrupt_test.bin",
			Size:   int64(len(testContent)),
			Reader: bytes.NewReader(testContent),
		},
	}

	receiverHandler := newTestHandler()

	// Use small block size to generate many subpackets
	senderCfg := &Config{MaxBlockSize: 512, Use32BitCRC: true}
	receiverCfg := &Config{MaxBlockSize: 512, Use32BitCRC: true}

	sender := NewSession(senderT, senderHandler, senderCfg)
	receiver := NewSession(receiverT, receiverHandler, receiverCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer w1.Close()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer w2.Close()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	received, ok := receiverHandler.receivedFiles["corrupt_test.bin"]
	if !ok {
		t.Fatal("corrupt_test.bin not received")
	}
	if !bytes.Equal(received.Bytes(), testContent) {
		t.Errorf("content mismatch: got %d bytes, want %d bytes", received.Len(), len(testContent))
	}
}

func TestRecvTimeoutDeadlineCapableTransport(t *testing.T) {
	// net.Pipe provides a synchronous, deadline-capable transport
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Drain c2 so the receiver's ZRINIT writes don't block on the synchronous pipe.
	// net.Pipe is unbuffered, so writes block until the other end reads.
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := c2.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	handler := newTestHandler()

	// Short timeout to make the test fast
	cfg := &Config{RecvTimeout: 50 * time.Millisecond}
	session := NewSession(c1, handler, cfg)

	start := time.Now()
	err := session.Receive(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	// The error surfaces as max retries exceeded (each retry triggers a deadline timeout)
	// or as a direct timeout error — either is acceptable.
	errStr := err.Error()
	if !strings.Contains(errStr, "timeout") && !strings.Contains(errStr, "deadline") &&
		!strings.Contains(errStr, "i/o timeout") && !strings.Contains(errStr, "max retries") {
		t.Fatalf("expected timeout or max retries error, got: %v", err)
	}

	// Should complete within a reasonable time (retries * timeout + overhead).
	// The receiver goes through srxInit (sends ZRINIT), then srxFileWait where
	// each recvHeader hits the timeout, retries MaxRetries times, plus the initial
	// ZRINIT send and consecutive error tracking.
	maxExpected := 5 * time.Second
	if elapsed > maxExpected {
		t.Errorf("took too long: %v (max expected %v)", elapsed, maxExpected)
	}
	t.Logf("completed in %v with error: %v", elapsed, err)
}

// TestLoopbackZCRCQCheckpoints tests that ZCRCQ checkpoints are emitted during
// streaming when the receiver advertises CANFDX.
func TestLoopbackZCRCQCheckpoints(t *testing.T) {
	// Create transports with a snooping wrapper to detect ZCRCQ in the stream
	r1, w1 := bufferedPipe(256)
	r2, w2 := bufferedPipe(256)

	zcrcqCount := atomic.Int32{}
	snoopW := &snoopingWriter{w: w1, onByte: func(prev, cur byte) {
		if prev == ZDLE && cur == ZCRCQ {
			zcrcqCount.Add(1)
		}
	}}

	senderT := &pipeReadWriter{Reader: r2, Writer: snoopW}
	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}

	// 32KB file — enough subpackets to trigger ZCRCQ at interval=8
	testContent := make([]byte, 32768)
	rand.Read(testContent)

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "zcrcq_test.bin",
			Size:   int64(len(testContent)),
			Reader: bytes.NewReader(testContent),
		},
	}

	receiverHandler := newTestHandler()

	// Small block size, receiver advertises CANFDX via default caps
	senderCfg := &Config{MaxBlockSize: 512}
	receiverCfg := &Config{MaxBlockSize: 512}

	sender := NewSession(senderT, senderHandler, senderCfg)
	receiver := NewSession(receiverT, receiverHandler, receiverCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer w1.Close()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer w2.Close()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	// Verify file was received correctly
	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()
	received, ok := receiverHandler.receivedFiles["zcrcq_test.bin"]
	if !ok {
		t.Fatal("zcrcq_test.bin not received")
	}
	if !bytes.Equal(received.Bytes(), testContent) {
		t.Error("content mismatch")
	}

	// With 32KB / starting 256-byte blocks growing to 512, there should be many subpackets,
	// and ZCRCQ should have been emitted at least once (every 8th subpacket after the first).
	if zcrcqCount.Load() == 0 {
		t.Error("expected at least one ZCRCQ checkpoint, got 0")
	}
	t.Logf("ZCRCQ checkpoints emitted: %d", zcrcqCount.Load())
}

// TestLoopbackWindowFlowControl tests the window flow control path where the
// receiver advertises a non-zero buffer size.
func TestLoopbackWindowFlowControl(t *testing.T) {
	r1, w1 := bufferedPipe(256)
	r2, w2 := bufferedPipe(256)

	senderT := &pipeReadWriter{Reader: r2, Writer: w1}
	receiverT := &pipeReadWriter{Reader: r1, Writer: w2}

	// 8KB file
	testContent := make([]byte, 8192)
	rand.Read(testContent)

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "window_test.bin",
			Size:   int64(len(testContent)),
			Reader: bytes.NewReader(testContent),
		},
	}

	receiverHandler := newTestHandler()

	senderCfg := &Config{MaxBlockSize: 512}
	// Receiver advertises 2048-byte window
	receiverCfg := &Config{MaxBlockSize: 512, WindowSize: 2048}

	sender := NewSession(senderT, senderHandler, senderCfg)
	receiver := NewSession(receiverT, receiverHandler, receiverCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer w1.Close()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer w2.Close()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()
	received, ok := receiverHandler.receivedFiles["window_test.bin"]
	if !ok {
		t.Fatal("window_test.bin not received")
	}
	if !bytes.Equal(received.Bytes(), testContent) {
		t.Error("content mismatch")
	}
}

func TestLoopbackResume(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	testContent := make([]byte, 4096)
	rand.Read(testContent)

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "resume.bin",
			Size:   int64(len(testContent)),
			Reader: bytes.NewReader(testContent),
		},
	}

	receiverHandler := newTestHandler()
	receiverHandler.acceptOffset = 1024 // resume from byte 1024

	sender := NewSession(senderTransport, senderHandler, &Config{MaxBlockSize: 512})
	receiver := NewSession(receiverTransport, receiverHandler, &Config{MaxBlockSize: 512})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	received, ok := receiverHandler.receivedFiles["resume.bin"]
	if !ok {
		t.Fatal("resume.bin not received")
	}
	// Only data from offset 1024 onwards should be in the buffer
	if !bytes.Equal(received.Bytes(), testContent[1024:]) {
		t.Errorf("content mismatch: got %d bytes, want %d bytes",
			received.Len(), len(testContent)-1024)
	}
	if err := receiverHandler.completedFiles["resume.bin"]; err != nil {
		t.Errorf("unexpected completion error: %v", err)
	}
}

func TestLoopbackMaxFileSize(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	smallContent := []byte("small file content here")
	bigContent := make([]byte, 5000)
	rand.Read(bigContent)
	mediumContent := []byte("medium file — should be received just fine!!")

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{Name: "small.txt", Size: int64(len(smallContent)), Reader: bytes.NewReader(smallContent)},
		{Name: "big.bin", Size: int64(len(bigContent)), Reader: bytes.NewReader(bigContent)},
		{Name: "medium.txt", Size: int64(len(mediumContent)), Reader: bytes.NewReader(mediumContent)},
	}

	receiverHandler := newTestHandler()

	sender := NewSession(senderTransport, senderHandler, nil)
	receiver := NewSession(receiverTransport, receiverHandler, &Config{MaxFileSize: 1000})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()

	// small and medium should be received
	if received, ok := receiverHandler.receivedFiles["small.txt"]; !ok {
		t.Error("small.txt not received")
	} else if !bytes.Equal(received.Bytes(), smallContent) {
		t.Error("small.txt content mismatch")
	}

	if received, ok := receiverHandler.receivedFiles["medium.txt"]; !ok {
		t.Error("medium.txt not received")
	} else if !bytes.Equal(received.Bytes(), mediumContent) {
		t.Error("medium.txt content mismatch")
	}

	// big file should NOT be received
	if _, ok := receiverHandler.receivedFiles["big.bin"]; ok {
		t.Error("big.bin should not have been received (exceeds MaxFileSize)")
	}

	// Sender should see ErrSkip for big file
	senderHandler.mu.Lock()
	defer senderHandler.mu.Unlock()
	if err := senderHandler.completedFiles["big.bin"]; err != ErrSkip {
		t.Errorf("expected ErrSkip for big.bin, got: %v", err)
	}
}

// readOnly wraps a reader to strip io.ReadSeeker, exposing only io.Reader.
type readOnly struct{ io.Reader }

func TestLoopbackNonSeekableZRPOS(t *testing.T) {
	senderTransport, receiverTransport, senderClose, receiverClose := newTestTransports()

	content1 := make([]byte, 2048)
	rand.Read(content1)
	content2 := make([]byte, 2048)
	rand.Read(content2)

	senderHandler := newTestHandler()
	senderHandler.filesToSend = []*FileOffer{
		{
			Name:   "nonseek.bin",
			Size:   int64(len(content1)),
			Reader: readOnly{bytes.NewReader(content1)}, // NOT seekable
		},
		{
			Name:   "seekable.bin",
			Size:   int64(len(content2)),
			Reader: bytes.NewReader(content2), // seekable
		},
	}

	receiverHandler := newTestHandler()
	receiverHandler.acceptOffset = 512 // forces non-zero ZRPOS

	sender := NewSession(senderTransport, senderHandler, &Config{MaxBlockSize: 512})
	receiver := NewSession(receiverTransport, receiverHandler, &Config{MaxBlockSize: 512})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sendErr, recvErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer senderClose()
		sendErr = sender.Send(ctx)
	}()
	go func() {
		defer wg.Done()
		defer receiverClose()
		recvErr = receiver.Receive(ctx)
	}()

	wg.Wait()

	if sendErr != nil {
		t.Fatalf("sender error: %v", sendErr)
	}
	if recvErr != nil {
		t.Fatalf("receiver error: %v", recvErr)
	}

	// Sender should report error for non-seekable file
	senderHandler.mu.Lock()
	defer senderHandler.mu.Unlock()
	if err := senderHandler.completedFiles["nonseek.bin"]; err == nil {
		t.Error("expected sender error for nonseek.bin, got nil")
	} else if !strings.Contains(err.Error(), "not seekable") {
		t.Errorf("expected 'not seekable' error, got: %v", err)
	}

	// Receiver should mark file 1 as completed with ErrSkip, buffer empty
	receiverHandler.mu.Lock()
	defer receiverHandler.mu.Unlock()
	if err := receiverHandler.completedFiles["nonseek.bin"]; err != ErrSkip {
		t.Errorf("expected receiver ErrSkip for nonseek.bin, got: %v", err)
	}
	if buf, ok := receiverHandler.receivedFiles["nonseek.bin"]; ok && buf.Len() != 0 {
		t.Errorf("expected empty buffer for nonseek.bin, got %d bytes", buf.Len())
	}

	// File 2 should be received (from offset 512 onwards)
	received, ok := receiverHandler.receivedFiles["seekable.bin"]
	if !ok {
		t.Fatal("seekable.bin not received")
	}
	if !bytes.Equal(received.Bytes(), content2[512:]) {
		t.Errorf("seekable.bin content mismatch: got %d bytes, want %d bytes",
			received.Len(), len(content2)-512)
	}
}

// snoopingWriter wraps a writer and calls onByte for each consecutive byte pair.
type snoopingWriter struct {
	w      io.Writer
	onByte func(prev, cur byte)
	prev   byte
}

func (sw *snoopingWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		sw.onByte(sw.prev, b)
		sw.prev = b
	}
	return sw.w.Write(p)
}

