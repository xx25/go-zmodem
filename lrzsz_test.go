package zmodem

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// findBinary returns the path to a binary or skips the test.
func findBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found in PATH, skipping integration test", name)
	}
	return path
}

// startRzReceiverWithBaseFlags launches rz with the given base flags plus extraFlags.
func startRzReceiverWithBaseFlags(t *testing.T, tempDir string, baseFlags, extraFlags []string) (net.Conn, *exec.Cmd) {
	t.Helper()
	rzPath := findBinary(t, "rz")

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("localhost:%d", port)

	args := []string{"--tcp-client", addr}
	args = append(args, baseFlags...)
	args = append(args, extraFlags...)

	cmd := exec.Command(rzPath, args...)
	cmd.Dir = tempDir
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		ln.Close()
		t.Fatalf("rz start: %v", err)
	}

	// Clean up listener and process on test failure
	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})

	conn, err := ln.Accept()
	if err != nil {
		cmd.Process.Kill()
		ln.Close()
		t.Fatalf("accept: %v", err)
	}
	ln.Close()

	return conn, cmd
}

// startRzReceiver launches rz --tcp-client in overwrite mode.
func startRzReceiver(t *testing.T, tempDir string, extraFlags []string) (net.Conn, *exec.Cmd) {
	t.Helper()
	return startRzReceiverWithBaseFlags(t, tempDir, []string{"-b", "-Z", "-q", "-O"}, extraFlags)
}

// startRzReceiverResume launches rz --tcp-client in resume/crash-recovery mode.
func startRzReceiverResume(t *testing.T, tempDir string, extraFlags []string) (net.Conn, *exec.Cmd) {
	t.Helper()
	return startRzReceiverWithBaseFlags(t, tempDir, []string{"-b", "-Z", "-q", "-r"}, extraFlags)
}

// startSzSender launches sz --tcp-client connecting to a TCP listener.
// Returns the accepted connection and the exec.Cmd.
func startSzSender(t *testing.T, files []string, extraFlags []string) (net.Conn, *exec.Cmd) {
	t.Helper()
	szPath := findBinary(t, "sz")

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	addr := fmt.Sprintf("localhost:%d", port)

	args := []string{"--tcp-client", addr, "-b", "-q"}
	args = append(args, extraFlags...)
	args = append(args, files...)

	cmd := exec.Command(szPath, args...)
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		ln.Close()
		t.Fatalf("sz start: %v", err)
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	})

	conn, err := ln.Accept()
	if err != nil {
		cmd.Process.Kill()
		ln.Close()
		t.Fatalf("accept: %v", err)
	}
	ln.Close()

	return conn, cmd
}

// createTestFile writes content to dir/name and returns the full path.
func createTestFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write test file %s: %v", path, err)
	}
	return path
}

// verifyFile reads the file at path and compares it to expected content.
func verifyFile(t *testing.T, path string, expected []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result file %s: %v", path, err)
	}
	if !bytes.Equal(got, expected) {
		t.Errorf("file %s content mismatch: got %d bytes, want %d bytes", filepath.Base(path), len(got), len(expected))
		if len(got) < 200 && len(expected) < 200 {
			t.Errorf("  got:  %q", got)
			t.Errorf("  want: %q", expected)
		}
	}
}

// lrzszFileHandler implements FileHandler for receiving files to a directory.
type lrzszFileHandler struct {
	dir       string
	files     []*FileOffer
	sendIdx   int
	completed map[string]error
	skipFiles map[string]bool
}

func newLrzszSendHandler(files []*FileOffer) *lrzszFileHandler {
	return &lrzszFileHandler{
		files:     files,
		completed: make(map[string]error),
	}
}

func newLrzszRecvHandler(dir string) *lrzszFileHandler {
	return &lrzszFileHandler{
		dir:       dir,
		completed: make(map[string]error),
		skipFiles: make(map[string]bool),
	}
}

func (h *lrzszFileHandler) NextFile() *FileOffer {
	if h.sendIdx >= len(h.files) {
		return nil
	}
	f := h.files[h.sendIdx]
	h.sendIdx++
	return f
}

func (h *lrzszFileHandler) AcceptFile(info FileInfo) (io.WriteCloser, int64, error) {
	name := SanitizeFilename(info.Name)
	if h.skipFiles[name] {
		return nil, 0, ErrSkip
	}
	path := filepath.Join(h.dir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, 0, err
	}
	return f, 0, nil
}

func (h *lrzszFileHandler) FileProgress(info FileInfo, bytesTransferred int64) {}

func (h *lrzszFileHandler) FileCompleted(info FileInfo, bytesTransferred int64, err error) {
	h.completed[info.Name] = err
}

// ==== Group A: Go Sender → lrzsz rz Receiver ====

func TestLrzszA1_SendSmallCRC16(t *testing.T) {
	recvDir := t.TempDir()
	content := []byte("Hello, ZMODEM integration test with lrzsz!")

	conn, cmd := startRzReceiver(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:    "small.txt",
			Size:    int64(len(content)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(content),
		},
	})

	session := NewSession(conn, handler, &Config{
		Use32BitCRC: false,
		MaxBlockSize: 1024,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "small.txt"), content)
}

func TestLrzszA2_SendCRC32(t *testing.T) {
	recvDir := t.TempDir()
	content := []byte("Testing CRC-32 mode with lrzsz receiver!")

	conn, cmd := startRzReceiver(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:    "crc32.txt",
			Size:    int64(len(content)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(content),
		},
	})

	session := NewSession(conn, handler, &Config{
		Use32BitCRC: true,
		MaxBlockSize: 1024,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "crc32.txt"), content)
}

func TestLrzszA3_SendBatch(t *testing.T) {
	recvDir := t.TempDir()

	files := []struct {
		name    string
		content []byte
	}{
		{"batch1.txt", []byte("First batch file")},
		{"batch2.dat", []byte("Second batch file with more content here")},
		{"batch3.bin", make([]byte, 2048)},
	}
	rand.Read(files[2].content)

	var offers []*FileOffer
	for _, f := range files {
		offers = append(offers, &FileOffer{
			Name:    f.name,
			Size:    int64(len(f.content)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(f.content),
		})
	}

	conn, cmd := startRzReceiver(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler(offers)
	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	for _, f := range files {
		verifyFile(t, filepath.Join(recvDir, f.name), f.content)
	}
}

func TestLrzszA4_SendEmpty(t *testing.T) {
	recvDir := t.TempDir()

	conn, cmd := startRzReceiver(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:    "empty.txt",
			Size:    0,
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader([]byte{}),
		},
	})

	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "empty.txt"), []byte{})
}

func TestLrzszA5_SendLarge(t *testing.T) {
	recvDir := t.TempDir()

	content := make([]byte, 256*1024) // 256KB
	rand.Read(content)

	conn, cmd := startRzReceiver(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:    "large.bin",
			Size:    int64(len(content)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(content),
		},
	})

	session := NewSession(conn, handler, &Config{MaxBlockSize: 8192})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "large.bin"), content)
}

func TestLrzszA6_SendBinary256(t *testing.T) {
	recvDir := t.TempDir()

	// All 256 byte values, repeated a few times
	content := make([]byte, 256*4)
	for i := range content {
		content[i] = byte(i % 256)
	}

	conn, cmd := startRzReceiver(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:    "allbytes.bin",
			Size:    int64(len(content)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(content),
		},
	})

	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "allbytes.bin"), content)
}

func TestLrzszA7_SendEscapeAll(t *testing.T) {
	recvDir := t.TempDir()
	content := []byte("Escape all control characters test data\x00\x01\x02\x03\x10\x11\x13\x1a\x7f\x80\x8d\x91\x93")

	conn, cmd := startRzReceiver(t, recvDir, []string{"-e"})
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:    "escaped.bin",
			Size:    int64(len(content)),
			ModTime: time.Now(),
			Mode:    0644,
			Reader:  bytes.NewReader(content),
		},
	})

	session := NewSession(conn, handler, &Config{
		EscapeAll:    true,
		MaxBlockSize: 1024,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "escaped.bin"), content)
}

// ==== Group B: lrzsz sz Sender → Go Receiver ====

func TestLrzszB1_RecvSmall(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()
	content := []byte("Hello from lrzsz sz sender!")
	srcPath := createTestFile(t, srcDir, "hello.txt", content)

	conn, cmd := startSzSender(t, []string{srcPath}, nil)
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "hello.txt"), content)
}

func TestLrzszB2_RecvCRC32(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()
	content := []byte("CRC-32 mode receive test with lrzsz sz!")
	srcPath := createTestFile(t, srcDir, "crc32recv.txt", content)

	conn, cmd := startSzSender(t, []string{srcPath}, nil)
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	session := NewSession(conn, handler, &Config{
		Use32BitCRC: true,
		MaxBlockSize: 1024,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "crc32recv.txt"), content)
}

func TestLrzszB3_RecvBatch(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()

	files := []struct {
		name    string
		content []byte
	}{
		{"rbatch1.txt", []byte("Received batch file 1")},
		{"rbatch2.dat", []byte("Received batch file 2 with more data here")},
		{"rbatch3.bin", make([]byte, 2048)},
	}
	rand.Read(files[2].content)

	var paths []string
	for _, f := range files {
		p := createTestFile(t, srcDir, f.name, f.content)
		paths = append(paths, p)
	}

	conn, cmd := startSzSender(t, paths, nil)
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	for _, f := range files {
		verifyFile(t, filepath.Join(recvDir, f.name), f.content)
	}
}

func TestLrzszB4_RecvLarge(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()

	content := make([]byte, 256*1024)
	rand.Read(content)
	srcPath := createTestFile(t, srcDir, "large.bin", content)

	conn, cmd := startSzSender(t, []string{srcPath}, nil)
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	session := NewSession(conn, handler, &Config{MaxBlockSize: 8192})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "large.bin"), content)
}

func TestLrzszB5_RecvBinary256(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()

	content := make([]byte, 256*4)
	for i := range content {
		content[i] = byte(i % 256)
	}
	srcPath := createTestFile(t, srcDir, "allbytes.bin", content)

	conn, cmd := startSzSender(t, []string{srcPath}, nil)
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "allbytes.bin"), content)
}

func TestLrzszB6_RecvEscapeAll(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()
	content := []byte("Escape all receive test\x00\x01\x02\x03\x10\x11\x13\x1a\x7f\x80\x8d\x91\x93")
	srcPath := createTestFile(t, srcDir, "escaped.bin", content)

	conn, cmd := startSzSender(t, []string{srcPath}, []string{"-e"})
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	session := NewSession(conn, handler, &Config{
		EscapeAll:    true,
		MaxBlockSize: 1024,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	verifyFile(t, filepath.Join(recvDir, "escaped.bin"), content)
}

// ==== Group A: Additional Tests ====

func TestLrzszA8_SendResume(t *testing.T) {
	recvDir := t.TempDir()

	// 8KB test content
	fullContent := make([]byte, 8192)
	rand.Read(fullContent)

	// Pre-create a partial file (first 2048 bytes) to simulate interrupted transfer
	partialPath := filepath.Join(recvDir, "resume.bin")
	if err := os.WriteFile(partialPath, fullContent[:2048], 0644); err != nil {
		t.Fatalf("write partial file: %v", err)
	}

	// Start rz in resume mode (-r instead of -O)
	conn, cmd := startRzReceiverResume(t, recvDir, nil)
	defer conn.Close()

	handler := newLrzszSendHandler([]*FileOffer{
		{
			Name:   "resume.bin",
			Size:   int64(len(fullContent)),
			Reader: bytes.NewReader(fullContent),
		},
	})

	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Send(ctx); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("rz exit error: %v", err)
	}

	verifyFile(t, partialPath, fullContent)
}

// ==== Group B: Additional Tests ====

func TestLrzszB7_RecvSkipInBatch(t *testing.T) {
	srcDir := t.TempDir()
	recvDir := t.TempDir()

	files := []struct {
		name    string
		content []byte
	}{
		{"keep1.txt", []byte("First file — should be received")},
		{"skip_me.txt", []byte("This file should be skipped by the Go receiver")},
		{"keep2.txt", []byte("Third file — should also be received")},
	}

	var paths []string
	for _, f := range files {
		p := createTestFile(t, srcDir, f.name, f.content)
		paths = append(paths, p)
	}

	conn, cmd := startSzSender(t, paths, nil)
	defer conn.Close()

	handler := newLrzszRecvHandler(recvDir)
	handler.skipFiles["skip_me.txt"] = true

	session := NewSession(conn, handler, &Config{MaxBlockSize: 1024})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := session.Receive(ctx); err != nil {
		t.Fatalf("Receive error: %v", err)
	}
	conn.Close()

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sz exit error: %v", err)
	}

	// Verify kept files
	verifyFile(t, filepath.Join(recvDir, "keep1.txt"), files[0].content)
	verifyFile(t, filepath.Join(recvDir, "keep2.txt"), files[2].content)

	// Verify skipped file was NOT written to disk
	if _, err := os.Stat(filepath.Join(recvDir, "skip_me.txt")); err == nil {
		t.Error("skip_me.txt should not exist on disk")
	}

	// Verify completion status
	if err := handler.completed["skip_me.txt"]; err != ErrSkip {
		t.Errorf("expected ErrSkip for skip_me.txt, got: %v", err)
	}
}
