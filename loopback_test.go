package zmodem

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"sync"
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
