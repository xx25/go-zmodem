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
	// DetectMergedSubpackets guards the CRC-16 lost-ZDLE merge detector
	// (detectMergedSubpacketCRC16). When a peer that cannot do CRC-32 transfers
	// over a link that drops bytes locally (e.g. a flaky RS232/USB serial path),
	// a lost ZDLE merges two subpackets and the CRC-16 residue property lets the
	// corrupt frame pass — silent corruption. With this set the receiver detects
	// the embedded frame and re-gets it. Default off: the recovery re-gets a
	// good-CRC subpacket mid-stream (a ZRPOS resync), which can briefly storm on
	// stale in-flight data, so it MUST be paired with a progress-aware
	// DataStallTimeout (>0) — never the legacy count budget — or a rare false
	// positive could exhaust it. Only meaningful for CRC-16 sessions.
	DetectMergedSubpackets bool
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
	// DataStallTimeout: progress-aware data-phase abort window. When > 0, a
	// mid-stream transfer is aborted only if it makes NO progress (no valid data
	// subpacket received) for this long — instead of after a fixed count of
	// consecutive errors. A noisy-but-advancing link (frequent CRC errors with
	// good subpackets in between, each of which resets the timer) therefore keeps
	// going as long as it advances, while a genuinely dead transfer still aborts.
	// 0 ⇒ the legacy count-based budget (dataRetryBudget) applies, unchanged. The
	// maxConsecutiveErr "peer not ZMODEM" guard is the pure-garbage backstop in
	// both modes.
	DataStallTimeout time.Duration
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
	// DataStallTimeout is left as supplied: 0 means "use the legacy count-based
	// budget", a deliberate opt-in for the progress-aware abort.
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

	// lastProgressAt is the clock time of the most recent valid data subpacket,
	// used by the progress-aware data-phase abort (Config.DataStallTimeout). It is
	// (re)set on entry to the data phase and on every good-CRC subpacket, so the
	// stall window measures "time since the transfer last made progress".
	lastProgressAt time.Time

	// mergeSuspectOffset is the write offset at which a suspected lost-ZDLE
	// merged subpacket (CRC-16) was last rejected. If the re-sent subpacket at
	// the same offset trips the detector again it is the SAME legit bytes (a
	// false positive, not a real merge), so it is accepted to avoid a stall
	// loop. -1 = none outstanding. See detectMergedSubpacketCRC16.
	mergeSuspectOffset int64

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
		transport:          transport,
		handler:            handler,
		cfg:                c,
		logger:             logger,
		tw:                 newTransportWriter(transport, c.EscapeMode),
		tr:                 newTransportReader(transport, c.GarbageThreshold, c.RecvTimeout, c.EscapeMode != EscapeMinimal, logger),
		mergeSuspectOffset: -1,
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
