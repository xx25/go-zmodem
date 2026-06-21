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

// DefaultRecvTimeout is the idle read timeout applied when NewSession is
// called with a nil Config. It is exported so callers that synthesize a
// Config (e.g. to inject a logger) can replicate the nil-config behaviour
// exactly: a supplied Config{RecvTimeout: 0} means "disabled", so a caller
// turning nil into an explicit Config must seed this value or it would
// silently lose the read timeout.
const DefaultRecvTimeout = 10 * time.Second

// Data-phase recovery (quiesce-drain) defaults. When a mid-stream data error
// hits and the transport supports read deadlines, the receiver drains the
// in-flight backlog until the sender pauses, then sends exactly one ZRPOS into
// the silence (see runReceiver). These bound that drain so a sender that never
// goes quiet — one streaming continuously to EOF while ignoring the
// reverse-channel ZRPOS — fails cleanly and bounded instead of forever.
const (
	// DefaultDataRecoveryQuietGap is the read-gap that signals the sender has
	// paused (finished its in-flight window). It must be longer than the normal
	// inter-burst gap of a streaming sender so steady streaming is not mistaken
	// for quiescence, yet far shorter than RecvTimeout so a real pause is caught
	// promptly.
	DefaultDataRecoveryQuietGap = 1500 * time.Millisecond
	// DefaultDataRecoveryMaxBytes caps the bytes discarded per recovery before
	// the drain gives up on a never-quiet sender. Generous enough to absorb a
	// large in-flight window at low baud, bounded enough to fail in seconds.
	DefaultDataRecoveryMaxBytes = 512 * 1024
	// DefaultDataRecoveryMaxWall caps the wall-clock spent draining per recovery,
	// catching a sub-gap trickle that would never trip the byte cap quickly.
	DefaultDataRecoveryMaxWall = 30 * time.Second
)

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
	// DataRecvTimeout: idle read timeout used DURING the data phase (while
	// receiving ZDATA subpackets), in place of RecvTimeout. 0 means "use
	// RecvTimeout". A value larger than RecvTimeout lets a brief mid-stream
	// flow-control pause (a V.42bis/LAPM buffer stall, an XON/XOFF hold) ride out
	// without tripping an error-recovery resync, while the shorter RecvTimeout
	// still bounds the control phases (waiting for ZFILE, ZEOF, ZRINIT). Safe to
	// lengthen even on a real modem: a dropped carrier is detected out-of-band
	// (DCD poll) regardless of how long this timeout is, so a longer wait only
	// delays recovery on a live-but-quiet line, never on a dead one.
	DataRecvTimeout time.Duration
	// Capabilities: receiver capability flags to advertise
	Capabilities byte
	// MaxFileSize: maximum accepted file size (0 = unlimited)
	MaxFileSize int64
	// MaxRetries: maximum retransmission attempts before abort (default 10)
	MaxRetries int
	// GarbageThreshold: max garbage bytes before aborting (default 1200)
	GarbageThreshold int
	// DataRecoveryQuietGap: read-gap that signals the sender has paused, used by
	// the data-phase quiesce-drain recovery. 0 → DefaultDataRecoveryQuietGap.
	// Effective only on a deadline-capable transport (e.g. net.Conn / modem).
	DataRecoveryQuietGap time.Duration
	// DataRecoveryMaxBytes: absolute cap on bytes discarded per recovery drain
	// before aborting a never-quiet sender. 0 → DefaultDataRecoveryMaxBytes.
	DataRecoveryMaxBytes int64
	// DataRecoveryMaxWall: absolute cap on wall-clock per recovery drain before
	// aborting a never-quiet sender. 0 → DefaultDataRecoveryMaxWall.
	DataRecoveryMaxWall time.Duration
	// Znulls: number of null bytes before ZDATA headers (default 0)
	Znulls int
	// Logger: optional structured logger for frame traces (recv/send headers,
	// ZDATA position mismatches, ZRPOS resync, garbage-skip diagnostics). When
	// nil, slog.Default() is used. Lets the caller route the protocol-level
	// trace into the same stream as its transport/byte trace.
	Logger *slog.Logger
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
	if c.DataRecoveryQuietGap <= 0 {
		c.DataRecoveryQuietGap = DefaultDataRecoveryQuietGap
	}
	if c.DataRecoveryMaxBytes <= 0 {
		c.DataRecoveryMaxBytes = DefaultDataRecoveryMaxBytes
	}
	if c.DataRecoveryMaxWall <= 0 {
		c.DataRecoveryMaxWall = DefaultDataRecoveryMaxWall
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
		c.RecvTimeout = DefaultRecvTimeout
	}

	logger := slog.Default()
	if c.Logger != nil {
		logger = c.Logger
	}

	s := &Session{
		transport: transport,
		handler:   handler,
		cfg:       c,
		logger:    logger,
		tw:        newTransportWriter(transport, c.EscapeMode),
		tr:        newTransportReader(transport, c.GarbageThreshold, c.RecvTimeout, c.EscapeMode != EscapeMinimal, logger),
	}
	// Seed the attention sequence from config so a receiver has a default Attn to
	// interrupt a streaming sender even when the peer sends no ZSINIT to negotiate
	// one; a ZSINIT, if it arrives, overwrites this (see runReceiver).
	s.attnSeq = c.AttnSequence
	// The data phase may use a longer idle read timeout than the control phases.
	s.tr.dataTimeout = c.DataRecvTimeout
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
