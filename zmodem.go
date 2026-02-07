package zmodem

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ErrSkip is returned by AcceptFile to skip a file.
var ErrSkip = errors.New("skip file")

// FileHandler is the application callback interface for file operations.
type FileHandler interface {
	// NextFile returns the next file to send, or nil if no more files.
	NextFile() *FileOffer

	// AcceptFile decides whether to accept an incoming file.
	// Return (writer, offset, nil) to accept starting at offset.
	// Return (nil, 0, ErrSkip) to skip the file.
	//
	// SECURITY: The caller MUST sanitize info.Name before using it as a
	// filesystem path. Incoming filenames may contain "../" path traversal.
	AcceptFile(info FileInfo) (io.WriteCloser, int64, error)

	// FileProgress is called periodically during transfer with the current byte count.
	FileProgress(info FileInfo, bytesTransferred int64)

	// FileCompleted is called when a file transfer finishes (success or error).
	FileCompleted(info FileInfo, bytesTransferred int64, err error)
}

// FileOffer describes a file to send.
type FileOffer struct {
	Name    string
	Size    int64
	ModTime time.Time
	Mode    uint32
	// Reader provides file data. If it implements io.ReadSeeker, resume via
	// ZRPOS is supported. If it only implements io.Reader, ZRPOS with non-zero
	// offset will cause the file to be skipped.
	Reader io.Reader
}

// FileInfo describes an incoming file (parsed from ZFILE subpacket).
type FileInfo struct {
	Name           string
	Size           int64
	ModTime        time.Time
	Mode           uint32
	FilesRemaining int
	BytesRemaining int64
}

// Config controls session behavior.
type Config struct {
	// MaxBlockSize: data subpacket size (default 1024, max 8192 for ZedZap)
	MaxBlockSize int
	// WindowSize: streaming window size (0 = full streaming, >0 = windowed)
	WindowSize int
	// EscapeMode controls ZDLE escaping: EscapeStandard (default), EscapeAll, or EscapeMinimal (DirZap).
	EscapeMode EscapeMode
	// Use32BitCRC: prefer CRC-32 when receiver supports it
	Use32BitCRC bool
	// AttnSequence: attention string for interrupting sender (max 32 bytes)
	AttnSequence []byte
	// RecvTimeout: idle timeout for reads from the remote.
	//
	// 0 disables deadline management. This is useful if the caller manages read
	// deadlines externally (e.g. on net.Conn) or the transport provides its own
	// timeout/cancellation mechanism.
	//
	// If Config is nil, RecvTimeout defaults to 10s.
	//
	// Effective only when the transport implements SetReadDeadline (e.g. net.Conn).
	// When enabled (>0), this overwrites any existing read deadline on the transport
	// while the session is running (cleared on exit).
	// For transports without deadline support, callers must handle cancellation
	// externally (e.g. by closing the transport).
	RecvTimeout time.Duration
	// Capabilities: receiver capability flags to advertise
	Capabilities byte
	// MaxFileSize: maximum accepted file size (0 = unlimited)
	MaxFileSize int64
	// MaxRetries: maximum retransmission attempts before abort (default 10)
	MaxRetries int
	// GarbageThreshold: max garbage bytes before aborting (default 1200)
	GarbageThreshold int
	// Znulls: number of null bytes before ZDATA headers (default 0)
	Znulls int
}

func (c *Config) defaults() {
	if c.MaxBlockSize <= 0 {
		c.MaxBlockSize = 1024
	}
	if c.MaxBlockSize > 8192 {
		c.MaxBlockSize = 8192
	}
	if c.RecvTimeout < 0 {
		c.RecvTimeout = 0
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 10
	}
	if c.GarbageThreshold <= 0 {
		c.GarbageThreshold = 1200
	}
}

// Session represents a ZMODEM transfer session over a transport.
type Session struct {
	transport io.ReadWriter
	handler   FileHandler
	cfg       Config
	logger    *slog.Logger

	tw *transportWriter
	tr *transportReader

	// Protocol state
	useCRC32         bool   // negotiated CRC mode
	remoteFlags      byte   // remote ZRINIT ZF0 flags
	remoteEscAll     bool   // remote wants all control chars escaped
	attnSeq          []byte // negotiated attention sequence
	remoteWindowSize int    // receiver buffer size from ZRINIT (ZP0+ZP1)

	mu     sync.Mutex
	active bool // prevents concurrent Send/Receive
}

// NewSession creates a new ZMODEM session over the given transport.
func NewSession(transport io.ReadWriter, handler FileHandler, cfg *Config) *Session {
	var c Config
	if cfg != nil {
		c = *cfg
	}
	c.defaults()
	if cfg == nil && c.RecvTimeout == 0 {
		// Keep the default behavior safe for net.Conn-like transports. If the caller
		// supplies a Config explicitly, RecvTimeout=0 means "disabled".
		c.RecvTimeout = 10 * time.Second
	}

	logger := slog.Default()

	s := &Session{
		transport: transport,
		handler:   handler,
		cfg:       c,
		logger:    logger,
		tw:        newTransportWriter(transport, c.EscapeMode),
		tr:        newTransportReader(transport, c.GarbageThreshold, c.RecvTimeout, c.EscapeMode != EscapeMinimal, logger),
	}
	return s
}

// Send initiates a file sending session (batch upload).
func (s *Session) Send(ctx context.Context) error {
	if !s.acquire() {
		return errors.New("zmodem: session already active")
	}
	defer s.release()
	defer s.tr.clearDeadline()
	return s.runSender(ctx)
}

// Receive initiates a file receiving session (batch download).
func (s *Session) Receive(ctx context.Context) error {
	if !s.acquire() {
		return errors.New("zmodem: session already active")
	}
	defer s.release()
	defer s.tr.clearDeadline()
	return s.runReceiver(ctx)
}

// Abort sends the abort sequence and terminates the session.
func (s *Session) Abort() error {
	_, err := s.transport.Write(abortSequence)
	return err
}

func (s *Session) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return false
	}
	s.active = true
	return true
}

func (s *Session) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
}
