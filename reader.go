package zmodem

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

var (
	errGarbageOverflow = errors.New("zmodem: garbage count exceeded threshold")
	errAbortReceived   = errors.New("zmodem: session aborted by remote (5x CAN)")
	errUnsupportedEnc  = errors.New("zmodem: unsupported frame encoding")
)

// deadlineSetter is implemented by transports that support read deadlines (e.g. net.Conn).
type deadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// transportReader wraps an io.Reader with buffering, ZDLE decoding,
// XON/XOFF stripping, and garbage counting.
type transportReader struct {
	r            *bufio.Reader
	ds           deadlineSetter // nil if transport lacks deadline support
	timeout      time.Duration  // idle timeout (from Config.RecvTimeout)
	garbageCount int
	garbageMax   int
	canCount     int // consecutive CAN characters seen
	logger       *slog.Logger
}

func newTransportReader(r io.Reader, garbageMax int, timeout time.Duration, logger *slog.Logger) *transportReader {
	tr := &transportReader{
		r:          bufio.NewReaderSize(r, 4096),
		timeout:    timeout,
		garbageMax: garbageMax,
		logger:     logger,
	}
	if ds, ok := r.(deadlineSetter); ok {
		tr.ds = ds
	}
	return tr
}

// readByte reads one raw byte from the transport.
// When the bufio buffer is empty and a deadline-capable transport is present,
// sets an idle timeout before blocking on the underlying read.
func (tr *transportReader) readByte() (byte, error) {
	if tr.r.Buffered() == 0 && tr.ds != nil && tr.timeout > 0 {
		tr.ds.SetReadDeadline(time.Now().Add(tr.timeout))
	}
	return tr.r.ReadByte()
}

// readByteStrip reads one byte, stripping XON/XOFF.
func (tr *transportReader) readByteStrip() (byte, error) {
	for {
		b, err := tr.readByte()
		if err != nil {
			return 0, err
		}
		// Strip XON/XOFF and their parity variants
		switch b & 0x7f {
		case XON, XOFF:
			continue
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
	if tr.ds != nil && tr.timeout > 0 {
		_ = tr.ds.SetReadDeadline(time.Time{})
	}
}

// purge reads and discards data for a short duration to clear stale transport data.
// Used before sending ZRPOS in error recovery.
func (tr *transportReader) purge() {
	// Discard whatever is buffered
	n := tr.r.Buffered()
	if n > 0 {
		tr.r.Discard(n)
	}
}
