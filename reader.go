package zmodem

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"
)

var (
	errGarbageOverflow = errors.New("zmodem: garbage count exceeded threshold")
	errAbortReceived   = errors.New("zmodem: session aborted by remote (5x CAN)")
	errUnsupportedEnc  = errors.New("zmodem: unsupported frame encoding")

	// errDrainByteCap and errDrainWallCap end a quiesce-drain that never reached
	// silence because the sender kept streaming. They are the bounded, clean
	// failure the data-phase recovery returns instead of discarding forever when
	// a peer ignores the reverse-channel ZRPOS and streams continuously to EOF.
	errDrainByteCap = errors.New("zmodem: data-recovery drain exceeded byte cap (sender streaming without re-framing)")
	errDrainWallCap = errors.New("zmodem: data-recovery drain exceeded time cap (sender streaming without re-framing)")
)

// deadlineSetter is implemented by transports that support read deadlines (e.g. net.Conn).
type deadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// transportReader wraps an io.Reader with buffering, ZDLE decoding,
// optional XON/XOFF stripping, and garbage counting.
type transportReader struct {
	r            *bufio.Reader
	ds           deadlineSetter // nil if transport lacks deadline support
	timeout      time.Duration  // idle timeout for control phases (Config.RecvTimeout)
	dataTimeout  time.Duration  // idle timeout for the data phase (Config.DataRecvTimeout); 0 → use timeout
	inDataPhase  bool           // true while receiving ZDATA subpackets; selects dataTimeout
	garbageCount int
	garbageMax   int
	canCount     int // consecutive CAN characters seen
	stripXonXoff bool
	logger       *slog.Logger
	now          func() time.Time // wall clock; overridable in tests for deterministic drain caps
}

func newTransportReader(r io.Reader, garbageMax int, timeout time.Duration, stripXonXoff bool, logger *slog.Logger) *transportReader {
	tr := &transportReader{
		r:            bufio.NewReaderSize(r, 4096),
		timeout:      timeout,
		garbageMax:   garbageMax,
		stripXonXoff: stripXonXoff,
		logger:       logger,
		now:          time.Now,
	}
	if ds, ok := r.(deadlineSetter); ok {
		tr.ds = ds
	}
	return tr
}

// activeTimeout is the idle read timeout for the current phase: the longer
// data-phase timeout while receiving ZDATA subpackets (if configured), else the
// control-phase timeout.
func (tr *transportReader) activeTimeout() time.Duration {
	if tr.inDataPhase && tr.dataTimeout > 0 {
		return tr.dataTimeout
	}
	return tr.timeout
}

// setDataPhase marks whether the receiver is currently in the data phase, which
// selects the data-phase read timeout for subsequent blocking reads.
func (tr *transportReader) setDataPhase(on bool) { tr.inDataPhase = on }

// readByte reads one raw byte from the transport.
// When the bufio buffer is empty and a deadline-capable transport is present,
// sets an idle timeout before blocking on the underlying read.
func (tr *transportReader) readByte() (byte, error) {
	if to := tr.activeTimeout(); tr.r.Buffered() == 0 && tr.ds != nil && to > 0 {
		tr.ds.SetReadDeadline(time.Now().Add(to))
	}
	return tr.r.ReadByte()
}

// readByteStrip reads one byte, optionally stripping XON/XOFF.
func (tr *transportReader) readByteStrip() (byte, error) {
	for {
		b, err := tr.readByte()
		if err != nil {
			return 0, err
		}
		if tr.stripXonXoff {
			// Strip XON/XOFF and their parity variants
			switch b & 0x7f {
			case XON, XOFF:
				continue
			}
		}
		return b, nil
	}
}

// zdlRead reads one ZDLE-decoded byte from the transport.
// Returns (byte, frameEnd, error) where frameEnd is non-zero if a
// subpacket end marker (ZCRCE/ZCRCG/ZCRCQ/ZCRCW) was encountered.
func (tr *transportReader) zdlRead() (byte, byte, error) {
	for {
		b, err := tr.readByteStrip()
		if err != nil {
			return 0, 0, err
		}

		if b == ZDLE { // ZDLE == CAN == 0x18
			tr.canCount++
			if tr.canCount >= 5 {
				return 0, 0, errAbortReceived
			}
			return tr.zdlEscape()
		}

		tr.canCount = 0
		return b, 0, nil
	}
}

// zdlEscape processes a byte after ZDLE prefix.
func (tr *transportReader) zdlEscape() (byte, byte, error) {
	c, err := tr.readByteStrip()
	if err != nil {
		return 0, 0, err
	}
	switch {
	case c == ZCRCE, c == ZCRCG, c == ZCRCQ, c == ZCRCW:
		// Subpacket end marker
		tr.canCount = 0
		return 0, c, nil

	case c == ZRUB0:
		tr.canCount = 0
		return 0x7f, 0, nil

	case c == ZRUB1:
		tr.canCount = 0
		return 0xff, 0, nil

	case c >= 0x40:
		// Standard escape: XOR with 0x40 to recover original
		tr.canCount = 0
		return c ^ 0x40, 0, nil

	default:
		// ZDLE followed by raw control char — noise/garbage.
		if c == CAN {
			tr.canCount++ // ZDLE already counted; CAN adds another
			if tr.canCount >= 5 {
				return 0, 0, errAbortReceived
			}
		}
		tr.logger.Debug("ZDLE noise: discarding", "byte", fmt.Sprintf("0x%02x", c))
		return tr.zdlRead() // recurse to read next valid byte
	}
}

// readHex reads two hex digits and returns the byte value.
// Strips parity bit (mask 0x7F) per lrzsz noxrd7() convention.
func (tr *transportReader) readHex() (byte, error) {
	hi, err := tr.readByte()
	if err != nil {
		return 0, err
	}
	lo, err := tr.readByte()
	if err != nil {
		return 0, err
	}
	hi &= 0x7f // strip parity
	lo &= 0x7f
	h, ok1 := hexVal(hi)
	l, ok2 := hexVal(lo)
	if !ok1 || !ok2 {
		return 0, fmt.Errorf("zmodem: invalid hex digits: 0x%02x 0x%02x", hi, lo)
	}
	return (h << 4) | l, nil
}

func hexVal(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		// Tolerate uppercase on receive (lenient)
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

// scanForPad scans input for a frame start (ZPAD + ZDLE + encoding byte).
// Returns the encoding type byte (ZBIN, ZHEX, ZBIN32, etc.).
// Tracks garbage count and returns error if threshold exceeded.
func (tr *transportReader) scanForPad() (byte, error) {
	tr.canCount = 0
	// garbageMax is the budget for ONE header hunt, not a session lifetime
	// total. Resetting it here lets each scan skip up to garbageMax bytes of
	// noise looking for a frame start. Without this reset the counter latches
	// after the first overflow and every later scan trips the threshold on its
	// first byte, so mid-stream resync (drain the in-flight backlog after a
	// data error, then catch the peer's ZRPOS/ZDATA) becomes impossible: the
	// receiver's retry budget is spent in milliseconds instead of spanning the
	// round-trips the drain actually needs.
	tr.garbageCount = 0

	for {
		b, err := tr.readByte()
		if err != nil {
			return 0, err
		}

		// Track CAN for abort detection
		if b == CAN {
			tr.canCount++
			if tr.canCount >= 5 {
				return 0, errAbortReceived
			}
			tr.garbageCount++
			if tr.garbageCount > tr.garbageMax {
				return 0, errGarbageOverflow
			}
			continue
		}
		tr.canCount = 0

		if b != ZPAD {
			// Not a pad character — garbage
			tr.garbageCount++
			if tr.garbageCount > tr.garbageMax {
				return 0, errGarbageOverflow
			}
			continue
		}

		// Got ZPAD. May have a second ZPAD (optional).
		b, err = tr.readByte()
		if err != nil {
			return 0, err
		}
		if b == ZPAD {
			// Second ZPAD — read next
			b, err = tr.readByte()
			if err != nil {
				return 0, err
			}
		}

		if b != ZDLE {
			tr.garbageCount++
			if tr.garbageCount > tr.garbageMax {
				return 0, errGarbageOverflow
			}
			continue
		}

		// Got ZDLE — next byte is encoding type
		enc, err := tr.readByte()
		if err != nil {
			return 0, err
		}

		switch enc {
		case ZBIN, ZHEX, ZBIN32:
			tr.garbageCount = 0 // valid frame start, reset garbage
			return enc, nil
		case ZBINR32, ZVBIN, ZVHEX, ZVBIN32, ZVBINR32:
			return 0, fmt.Errorf("%w: 0x%02x", errUnsupportedEnc, enc)
		default:
			tr.garbageCount++
			if tr.garbageCount > tr.garbageMax {
				return 0, errGarbageOverflow
			}
			continue
		}
	}
}

// resetGarbage resets the garbage counter (after successful frame).
func (tr *transportReader) resetGarbage() {
	tr.garbageCount = 0
}

// peekForZPAD scans all currently buffered bytes for a ZPAD or CAN character.
// Returns true if a frame start or abort might be pending.
// This is purely opportunistic — it does not block or issue I/O.
func (tr *transportReader) peekForZPAD() bool {
	n := tr.r.Buffered()
	if n == 0 {
		return false
	}
	peek, err := tr.r.Peek(n)
	if err != nil {
		return false
	}
	for _, b := range peek {
		if b == ZPAD || b == CAN {
			return true
		}
	}
	return false
}

// clearDeadline removes any read deadline set on the transport.
// Called on session exit so callers can reuse the transport without stale deadlines.
func (tr *transportReader) clearDeadline() {
	if tr.ds != nil {
		_ = tr.ds.SetReadDeadline(time.Time{})
	}
}

// deadlineCapable reports whether the transport supports read deadlines and a
// positive idle timeout is configured for some phase — the precondition for the
// quiesce-drain recovery to sense a stream going quiet.
func (tr *transportReader) deadlineCapable() bool {
	return tr.ds != nil && (tr.timeout > 0 || tr.dataTimeout > 0)
}

// purge discards the bytes currently sitting in the bufio buffer to clear stale
// transport data before sending ZRPOS in error recovery. It only drops what is
// already buffered (it does not, and cannot non-blockingly, drain the OS/modem
// serial buffer), and it logs the discarded byte count so a frame trace can show
// whether a recovery cycle dropped a fresh inbound header or left stale in-flight
// bytes behind — the otherwise-invisible signal needed to diagnose a resync loop.
func (tr *transportReader) purge() {
	n := tr.r.Buffered()
	if n > 0 {
		tr.r.Discard(n)
	}
	tr.logger.Debug("purge: discarded buffered bytes", "count", n)
}

// drainToQuiet reads and discards bytes until the incoming stream goes quiet,
// then returns the number of bytes discarded with a nil error. "Quiet" means a
// read blocked for the whole quietGap without a byte arriving: that gap is the
// signal a streaming sender has finished its in-flight window and the line is
// now silent enough for a single reverse-channel ZRPOS to be noticed, instead of
// being lost in a continuous ZCRCG backlog.
//
// Every blocking read is fenced by a fresh quietGap read deadline, so the drain
// can never park indefinitely on a single read. Two absolute caps bound a sender
// that never goes quiet — one streaming continuously (or trickling below the gap)
// to EOF: once maxBytes have been discarded or maxWall has elapsed the drain
// stops and returns errDrainByteCap / errDrainWallCap, so the caller aborts the
// transfer cleanly and bounded rather than discarding forever. Any non-timeout
// transport error (carrier loss, closed port, EOF) is returned immediately so
// the caller aborts on it.
//
// Only meaningful on a deadline-capable transport; callers must check tr.ds != nil
// && tr.timeout > 0 before relying on the quiescence signal.
func (tr *transportReader) drainToQuiet(quietGap time.Duration, maxBytes int64, maxWall time.Duration) (int64, error) {
	var discarded int64

	// The bytes already sitting in the bufio buffer are part of the in-flight
	// backlog; drop them first and count them against the byte cap.
	if n := tr.r.Buffered(); n > 0 {
		_, _ = tr.r.Discard(n)
		discarded += int64(n)
	}

	start := tr.now()
	buf := make([]byte, 2048)
	for {
		if discarded >= maxBytes {
			tr.logger.Debug("drain: byte cap reached", "discarded", discarded, "cap", maxBytes)
			return discarded, errDrainByteCap
		}
		if tr.now().Sub(start) >= maxWall {
			tr.logger.Debug("drain: wall-clock cap reached", "discarded", discarded, "cap", maxWall)
			return discarded, errDrainWallCap
		}

		if tr.ds != nil {
			_ = tr.ds.SetReadDeadline(tr.now().Add(quietGap))
		}
		n, err := tr.r.Read(buf)
		if n > 0 {
			discarded += int64(n)
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// The read gap elapsed with no byte: the stream is quiescent.
				tr.logger.Debug("drain: stream quiesced", "discarded", discarded)
				return discarded, nil
			}
			// Carrier loss / closed transport / EOF: not a recoverable gap.
			return discarded, err
		}
	}
}
