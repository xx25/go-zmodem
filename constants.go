package zmodem

// Frame encoding types
const (
	ZPAD  = 0x2a // '*' — pad character, begins frames
	ZDLE  = 0x18 // Ctrl-X — data link escape
	ZDLEE = 0x58 // Escaped ZDLE (ZDLE XOR 0x40)
	ZBIN  = 0x41 // 'A' — binary frame (CRC-16)
	ZHEX  = 0x42 // 'B' — hex frame (CRC-16)

	ZBIN32  = 0x43 // 'C' — binary frame (CRC-32)
	ZBINR32 = 0x44 // 'D' — RLE binary frame (CRC-32)
)

// Variable-length header frame types (recognized, not supported)
const (
	ZVBIN    = 0x61 // 'a' — variable-length binary (CRC-16)
	ZVHEX    = 0x62 // 'b' — variable-length hex (CRC-16)
	ZVBIN32  = 0x63 // 'c' — variable-length binary (CRC-32)
	ZVBINR32 = 0x64 // 'd' — variable-length RLE binary (CRC-32)
)

// RLE escape character
const ZRESC = 0x7e

// Frame types (0x00-0x13, standard ZMODEM)
const (
	ZRQINIT    = 0x00 // Request receive init
	ZRINIT     = 0x01 // Receive init
	ZSINIT     = 0x02 // Send init sequence
	ZACK       = 0x03 // ACK
	ZFILE      = 0x04 // File name/info
	ZSKIP      = 0x05 // Skip this file
	ZNAK       = 0x06 // Last header garbled
	ZABORT     = 0x07 // Abort batch transfer
	ZFIN       = 0x08 // Finish session
	ZRPOS      = 0x09 // Resume at offset
	ZDATA      = 0x0a // Data follows
	ZEOF       = 0x0b // End of file
	ZFERR      = 0x0c // File I/O error
	ZCRC       = 0x0d // File CRC request/response
	ZCHALLENGE = 0x0e // Security challenge
	ZCOMPL     = 0x0f // Request complete
	ZCAN       = 0x10 // Pseudo: session aborted (5x CAN detected)
	ZFREECNT   = 0x11 // Request free disk space
	ZCOMMAND   = 0x12 // Remote command
	ZSTDERR    = 0x13 // Output to stderr
)

// Microsoft HyperTerminal extended frame types (0x14-0x1A)
const (
	ZBADFMT      = 0x14 // Data packet format error
	ZMDM_ACKED   = 0x15 // Reserved
	ZMDM_VIRUS   = 0x16 // Error due to virus
	ZMDM_REFUSE  = 0x17 // File refused, no reason given
	ZMDM_OLDER   = 0x18 // File refused — older than existing
	ZMDM_INUSE   = 0x19 // File is currently in use
	ZMDM_CARRIER = 0x1A // Lost carrier
)

// maxFrameType is the highest recognized frame type for bounds checking.
const maxFrameType = ZMDM_CARRIER

// AutoDownloadString is sent before ZRQINIT to trigger auto-receive
// in terminal emulators (minicom, Tera Term, etc.)
var AutoDownloadString = []byte("rz\r")

// Data subpacket end types (ZDLE sequences)
const (
	ZCRCE = 0x68 // CRC next, frame ends, header follows
	ZCRCG = 0x69 // CRC next, frame continues nonstop
	ZCRCQ = 0x6a // CRC next, frame continues, ZACK expected
	ZCRCW = 0x6b // CRC next, ZACK expected, end of frame
	ZRUB0 = 0x6c // Translate to 0x7f (DEL)
	ZRUB1 = 0x6d // Translate to 0xff
)

// Receiver capability flags (ZRINIT ZF0/ZF1)
const (
	CANFDX  = 0x01 // Full duplex
	CANOVIO = 0x02 // Can receive during disk I/O
	CANBRK  = 0x04 // Can send break signal
	CANCRY  = 0x08 // Can decrypt
	CANLZW  = 0x10 // Can decompress
	CANFC32 = 0x20 // Can use 32-bit CRC
	ESCCTL  = 0x40 // Expects control chars escaped
	ESC8    = 0x80 // Expects 8th bit escaped
)

// ZFILE conversion options (ZF0)
const (
	ZCBIN   = 1 // Binary transfer
	ZCNL    = 2 // Convert NL
	ZCRECOV = 3 // Resume interrupted transfer
)

// ZFILE management options (ZF1, lower 5 bits masked by ZMMASK)
const (
	ZMMASK    = 0x1f // Mask for management option bits
	ZMNEWL    = 1    // Transfer if newer or longer
	ZMCRC     = 2    // Transfer if different CRC
	ZMAPND    = 3    // Append to existing
	ZMCLOB    = 4    // Replace existing (clobber)
	ZMDIFF    = 5    // Transfer if different date/length
	ZMPROT    = 6    // Protect — only if absent
	ZMNEW     = 7    // Transfer if newer
	ZMCHNG    = 8    // Change filename if destination exists
	ZMSKNOLOC = 0x80 // Skip file if not present at receiver
)

// ZSINIT flags (ZF0)
const (
	TESCCTL = 0x40 // Transmitter expects ctl chars escaped
	TESC8   = 0x80 // Transmitter expects 8th bit escaped
)

// Attention string meta-characters (inside AttnSequence)
const (
	AttnBreak = 0xDD // Send break signal to remote
	AttnPause = 0xDE // Pause one second
)

// XON/XOFF flow control characters
const (
	XON  = 0x11
	XOFF = 0x13
)

// EscapeMode controls which bytes are ZDLE-escaped on the wire.
type EscapeMode int

const (
	EscapeStandard EscapeMode = iota // Standard ZMODEM/ZedZap (default)
	EscapeAll                        // Escape all control chars (hostile transports)
	EscapeMinimal                    // DirZap: escape only ZDLE (0x18)
)

// CAN is the cancel character; 5 consecutive CANs abort a session.
const CAN = 0x18

// abortSequence is 8x CAN + 10x BS per spec.
var abortSequence = []byte{
	0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18,
	0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08,
}

// frameTypeName returns a human-readable name for a frame type.
func frameTypeName(ft byte) string {
	switch ft {
	case ZRQINIT:
		return "ZRQINIT"
	case ZRINIT:
		return "ZRINIT"
	case ZSINIT:
		return "ZSINIT"
	case ZACK:
		return "ZACK"
	case ZFILE:
		return "ZFILE"
	case ZSKIP:
		return "ZSKIP"
	case ZNAK:
		return "ZNAK"
	case ZABORT:
		return "ZABORT"
	case ZFIN:
		return "ZFIN"
	case ZRPOS:
		return "ZRPOS"
	case ZDATA:
		return "ZDATA"
	case ZEOF:
		return "ZEOF"
	case ZFERR:
		return "ZFERR"
	case ZCRC:
		return "ZCRC"
	case ZCHALLENGE:
		return "ZCHALLENGE"
	case ZCOMPL:
		return "ZCOMPL"
	case ZCAN:
		return "ZCAN"
	case ZFREECNT:
		return "ZFREECNT"
	case ZCOMMAND:
		return "ZCOMMAND"
	case ZSTDERR:
		return "ZSTDERR"
	case ZBADFMT:
		return "ZBADFMT"
	case ZMDM_ACKED:
		return "ZMDM_ACKED"
	case ZMDM_VIRUS:
		return "ZMDM_VIRUS"
	case ZMDM_REFUSE:
		return "ZMDM_REFUSE"
	case ZMDM_OLDER:
		return "ZMDM_OLDER"
	case ZMDM_INUSE:
		return "ZMDM_INUSE"
	case ZMDM_CARRIER:
		return "ZMDM_CARRIER"
	default:
		return "UNKNOWN"
	}
}
