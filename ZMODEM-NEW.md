# ZMODEM Library Implementation Plan

## Goal

Create a reusable, idiomatic Go ZMODEM library implementing the full protocol specification, suitable for embedding in different projects (FidoNet mailers, BBS systems, terminal emulators, file transfer tools). Maximize use of external libraries to minimize code.

## Research Summary

### Sources Analyzed
- **Specifications**: `spec.md` (wiki.synchro.net), `zmodem.txt` (Chuck Forsberg/Omen Technology, Oct 1988)
- **Historic specs collection** (`zmodem_docs/`):
  - `bbsdoc/zmodem.txt` — original Forsberg spec (Rev Oct-14-88) with critical 1987 revision notes
  - `bbsdoc/ymodem.txt` — YMODEM spec (file metadata format that ZMODEM reuses verbatim)
  - `synchro/zmodem.txt` — Synchronet BBS practical reference with real-world interop warnings
  - `zmodem_msht/` — Microsoft HyperTerminal ZMODEM docs (8 files covering each frame type)
  - `forums/understanding_zmodem.txt` — Stack Overflow implementor experience (pitfalls, debugging)
  - `mystic/m_protocol_zmodem.pas` — Mystic BBS Pascal implementation (1997-2013)
  - `qodem/zmodem.c` — Qodem terminal emulator C implementation (2003-2017, Kevin Lamonte, CC0)
  - `zmodem/zmodem.h` — Tsinghua Tongfang embedded C header (WaZOO/FidoNet constants)
- **Reference C implementation**: `lrzsz-0.12.20` (sz.c, rz.c, zm.c, zmodem.h, crctab.c) — ~5000 lines
- **Old Go implementation**: `github.com/xiwh/zmodem` (xiwh) — 17 confirmed bugs, incomplete features
- **8 FidoNet mailer implementations** examined:
  - **bforce** (C, Unix) — best structured, explicit state machines (ZTX_*/ZRX_* states)
  - **FTNd** (C, Linux) — full protocol with RLE, variable headers
  - **qico** (C, Unix) — clean standalone, ZedZap/DirZap support
  - **xenia** (C, DOS) — custom compression, brain-dead detection
  - **ifmail** (C, Unix) — conservative, close to Omen Technology reference
  - **BinkleyTerm XE** (C, DOS/OS2/Win32) — classic implementation
  - **Taurus/Argus** (Delphi) — Windows implementations
  - **Ravel FTN** (C, Mac Classic) — with Mac resource fork handling

### Key Lessons from Existing Implementations

1. **bforce** has the cleanest architecture — explicit state enums, table-driven escape, clear separation of concerns
2. All 8 mailer implementations agree on core protocol but differ in error recovery strategy
3. The old Go implementation (xiwh) has the right API idea (io.Writer + consumer callbacks) but broken protocol implementation
4. CRC16/CRC32 are available as Go packages — no need to implement tables
5. ZDLE encoding/decoding is ~50 lines when done correctly (table-driven like bforce)
6. Most complexity is in the state machines and error recovery, not in framing

### Historic Specs & Implementation Analysis Findings

Cross-referencing 8 historic documents (original Forsberg spec, YMODEM spec, Synchronet wiki, Microsoft HyperTerminal docs, Mystic BBS Pascal, Qodem C, Stack Overflow implementor reports) revealed critical details missing from earlier plan versions:

**Critical lrzsz interop workarounds** (from Qodem's battle-tested code):
1. **Force hex encoding for ZRPOS and ZCRC** sent by receiver — `sz` requires hex, ignores binary ZRPOS/ZCRC
2. **lrzsz assumes CRC-32 on ZSINIT** regardless of actual encoding — send ZSINIT as binary (not hex) when CRC-32 active
3. **ZSINIT data must escape control chars** even if `TX_ESCAPE_CTRL` is not globally set
4. **Validate ZRPOS position ≤ file size** before seeking — `sz` crashes on out-of-range ZRPOS

**Protocol correctness** (from 1987 spec revision & real-world):
5. **ZEOF with wrong offset must be IGNORED** — spec revision 07-31-1987 explicitly warns that answering ZEOF/offset-mismatch with ZRPOS causes double-retransmission loops
6. **Hex digits MUST be lowercase** — uppercase false-triggers XMODEM/YMODEM receivers
7. **`"rz\r"` auto-download string** before ZRQINIT — terminal emulators watch for this to auto-start receive
8. **Attention string meta-characters**: `\335` (octal 0xDD) = send break signal, `\336` (octal 0xDE) = pause 1 second
9. **Data subpacket CRC includes the end-type byte** — ZCRCE/ZCRCG/ZCRCQ/ZCRCW byte is fed into CRC before finalization
10. **ZDLE followed by raw control char = silently discard** — handles noise/garbage after ZDLE gracefully
11. **Microsoft HyperTerminal extended frame types 0x14-0x1A** — ZBADFMT, ZMDM_VIRUS, ZMDM_REFUSE, etc. Recognize and log, don't crash
12. **ZCRC with byte count 0** means "CRC the entire file" (not "zero bytes")
13. **ZCBIN does NOT override ZCRESUM** — exception to general sender-overrides-receiver rule (spec rev 6-24-88)

**Error recovery details** (from Mystic & Qodem):
14. **Buffer purge (drain ~100ms)** before sending ZRPOS — clear stale data from transport
15. **Consecutive error limit (~15) outside ZDATA** = peer not running ZMODEM, abort session
16. **Block size minimum is 32 bytes** (Qodem) or 64 bytes (Mystic); GoodNeeded threshold doubles on each error (caps at 16)
17. **"Lenient on receive, strict on transmit"** — Mystic's commented-out strict ZDLE validation proves strict receive breaks with real senders

**Compatibility warnings** (from Synchronet & Stack Overflow):
18. **CANFC32 is problematic** — Synchronet warns "most implementations balk at anything other than 0,0"; Telix sends 35-byte packets when receiver advertises CANFC32. Be prepared for pathological sender behavior.
19. **ZCRCW subpackets are NOT frames** — the single most common implementor confusion (from Stack Overflow). They are data subpacket terminators/footers, not headers or frames.
20. **Real-world ZMODEM libraries have ~5% failure rate** — emphasizes need for thorough interop testing, not just unit tests

### Codex Review Findings (Integrated)

The plan was reviewed by Codex which identified 10 critical/important issues now incorporated below:

1. **CRC-16 finalization** — ZMODEM CRC-16 requires feeding two zero bytes after data (lrzsz `updcrc(0, updcrc(0, crc))`). Bare `crc16.Checksum()` is WRONG.
2. **Non-blocking reverse channel** — Sender must sample reverse channel without blocking; requires a dedicated goroutine.
3. **HEX header parity bits** — lrzsz sends LF as 0x8A (parity set), Tera Term sends CR as 0x8D. Reader must strip bit 7.
4. **Write buffering** — Must use `bufio.Writer` internally; individual byte writes cause terrible syscall overhead.
5. **Missing constants** — ZMSKNOLOC, ZMCHNG, ZBINR32, ZVBIN/ZVHEX/ZVBIN32/ZVBINR32, ZRESC must all be defined.
6. **Path traversal security** — Incoming filenames can contain `../`; library must sanitize or document the risk.
7. **Progress callback** — Real applications need transfer progress; add `FileProgress` to `FileHandler`.
8. **Concurrent Send/Receive guard** — Must prevent calling both on same session; add mutex guard.
9. **Non-seekable reader handling** — If `FileOffer.Reader` is a pipe, resume is impossible; fail early or disable.
10. **Scope underestimate** — Error recovery paths expand code ~30%; budget ~2000 lines, not ~1500.

## Architecture

### Design Principles

1. **Transport-agnostic**: Protocol operates over any `io.ReadWriter` (serial port, TCP socket, PTY, pipe)
2. **Bidirectional**: Both sender and receiver in one library
3. **Callback-driven**: Application provides file I/O via interfaces, protocol handles framing
4. **Streaming**: No buffering entire files — data flows through as it arrives
5. **Explicit state machines**: Named states matching the spec (like bforce's ZTX_*/ZRX_* pattern)
6. **Context-aware**: Support Go `context.Context` for cancellation and timeouts
7. **Non-blocking reverse channel**: Sender uses a dedicated goroutine to read the reverse channel into a buffered channel, enabling non-blocking sampling during data transmission (like lrzsz's `rdchk()`)

### External Libraries

| Library | Purpose | Saves |
|---------|---------|-------|
| `github.com/sigurn/crc16` | CRC-16/XMODEM (CCITT) | CRC-16 table + calculation (~100 lines) |
| `hash/crc32` (stdlib) | CRC-32/IEEE | CRC-32 table + calculation (~100 lines) |
| `log/slog` (stdlib, Go 1.21+) | Structured logging | Debug/trace logging (no external dep) |

Note: Prefer `log/slog` over `zerolog` — it's in the stdlib, sufficient for protocol debugging, and avoids adding an external dependency to a library meant for embedding.

### Module Structure

```
github.com/<user>/zmodem/
├── go.mod
├── zmodem.go            # Public API: Session, Config, Options
├── constants.go         # Protocol constants (frame types, ZDLE sequences, capability flags)
├── crc.go               # CRC-16 and CRC-32 wrappers using external libs
├── escape.go            # ZDLE escape/unescape engine (table-driven)
├── frame.go             # Frame header marshal/unmarshal (HEX, ZBIN, ZBIN32)
├── subpacket.go         # Data subpacket marshal/unmarshal with CRC
├── fileinfo.go          # ZFILE metadata parsing/marshaling
├── sender.go            # Sender state machine
├── receiver.go          # Receiver state machine
├── reader.go            # Low-level transport reader (timeout, ZDLE-aware byte reading)
├── writer.go            # Low-level transport writer (buffered, escape-aware)
├── zmodem_test.go       # Integration tests
├── frame_test.go        # Frame round-trip tests
├── escape_test.go       # Escape/unescape tests
├── subpacket_test.go    # Subpacket tests
├── fileinfo_test.go     # File info parsing tests
├── loopback_test.go     # Sender↔Receiver loopback test
└── cmd/
    └── zmodem-test/
        └── main.go      # CLI test harness (interop testing with lrzsz)
```

## Public API

```go
package zmodem

import (
    "context"
    "io"
    "time"
)

// Session represents a ZMODEM transfer session over a transport.
// A Session is NOT safe for concurrent use. Do not call Send() and Receive()
// concurrently on the same Session — ZMODEM is a half-duplex protocol.
type Session struct { ... }

// Config controls session behavior.
type Config struct {
    // MaxBlockSize: data subpacket size (default 1024, max 8192 for ZedZap)
    MaxBlockSize int
    // WindowSize: streaming window size (0 = full streaming, >0 = windowed)
    WindowSize int
    // EscapeAll: escape all control characters (for hostile transports)
    EscapeAll bool
    // Use32BitCRC: prefer CRC-32 when receiver supports it
    Use32BitCRC bool
    // AttnSequence: attention string for interrupting sender (max 32 bytes)
    AttnSequence []byte
    // RecvTimeout: idle timeout waiting for data from remote (0 = disabled; default 10s if Config is nil)
    RecvTimeout time.Duration
    // Capabilities: receiver capability flags to advertise
    Capabilities byte
    // MaxFileSize: maximum accepted file size (0 = unlimited). Protects against
    // resource exhaustion from malicious senders claiming enormous file sizes.
    MaxFileSize int64
    // MaxRetries: maximum retransmission attempts before abort (default 10)
    MaxRetries int
    // GarbageThreshold: max garbage bytes before aborting (default 1200, like lrzsz)
    GarbageThreshold int
    // Znulls: number of null bytes to send before ZDATA headers.
    // Needed for legacy serial modems that require turnaround time. Default 0.
    Znulls int
}

// FileHandler is the application callback interface for file operations.
type FileHandler interface {
    // NextFile returns the next file to send, or nil if no more files.
    // Called by the sender to get files for batch transfer.
    NextFile() *FileOffer

    // AcceptFile decides whether to accept an incoming file.
    // Return (writer, offset, nil) to accept starting at offset.
    // Return (nil, 0, ErrSkip) to skip the file.
    // offset > 0 enables resume from that position.
    //
    // SECURITY: The caller MUST sanitize info.Name before using it as a
    // filesystem path. Incoming filenames may contain "../" path traversal
    // sequences. Use filepath.Base() or validate against an allowed directory.
    AcceptFile(info FileInfo) (io.WriteCloser, int64, error)

    // FileProgress is called periodically during transfer with the current
    // byte count. Applications can use this to display progress bars, ETA, etc.
    FileProgress(info FileInfo, bytesTransferred int64)

    // FileCompleted is called when a file transfer finishes (success or error).
    FileCompleted(info FileInfo, bytesTransferred int64, err error)
}

// FileOffer describes a file to send.
type FileOffer struct {
    Name    string
    Size    int64
    ModTime time.Time
    Mode    uint32       // Unix file mode (0 if not from Unix)
    // Reader provides file data. If it implements io.ReadSeeker, resume via
    // ZRPOS is supported. If it only implements io.Reader, ZRPOS with non-zero
    // offset will cause an error and the file will be skipped.
    Reader  io.Reader
}

// FileInfo describes an incoming file (parsed from ZFILE subpacket).
type FileInfo struct {
    Name           string
    Size           int64     // 0 if unknown (spec: "file length as an estimate only")
    ModTime        time.Time // Zero value if unknown
    Mode           uint32
    FilesRemaining int       // 0 if unknown
    BytesRemaining int64     // 0 if unknown
}

// ErrSkip is returned by AcceptFile to skip a file.
var ErrSkip = errors.New("skip file")

// NewSession creates a new ZMODEM session over the given transport.
func NewSession(transport io.ReadWriter, handler FileHandler, cfg *Config) *Session

// Send initiates a file sending session (batch upload).
// Blocks until all files are transferred or an error occurs.
// Only one of Send() or Receive() may be active at a time.
func (s *Session) Send(ctx context.Context) error

// Receive initiates a file receiving session (batch download).
// Blocks until the sender closes the session or an error occurs.
// Only one of Send() or Receive() may be active at a time.
func (s *Session) Receive(ctx context.Context) error

// Abort sends the abort sequence and terminates the session.
func (s *Session) Abort() error
```

## Protocol Implementation Details

### 1. Constants (`constants.go`)

All protocol constants from the spec:

```go
// Frame encoding types
const (
    ZPAD   = 0x2a // '*' — pad character, begins frames
    ZDLE   = 0x18 // Ctrl-X — data link escape
    ZDLEE  = 0x58 // Escaped ZDLE
    ZBIN   = 0x41 // 'A' — binary frame (CRC-16)
    ZHEX   = 0x42 // 'B' — hex frame (CRC-16)
    ZBIN32 = 0x43 // 'C' — binary frame (CRC-32)
    ZBINR32 = 0x44 // 'D' — RLE binary frame (CRC-32)
)

// Variable-length header frame types (must be recognized and rejected cleanly)
const (
    ZVBIN    = 0x61 // 'a' — variable-length binary (CRC-16)
    ZVHEX    = 0x62 // 'b' — variable-length hex (CRC-16)
    ZVBIN32  = 0x63 // 'c' — variable-length binary (CRC-32)
    ZVBINR32 = 0x64 // 'd' — variable-length RLE binary (CRC-32)
)

// RLE escape character
const ZRESC = 0x7e // Run length encoding flag/escape

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
// Not part of standard ZMODEM, but must be recognized and logged (not crash).
const (
    ZBADFMT         = 0x14 // Data packet format error (HyperTerminal)
    ZMDM_ACKED      = 0x15 // Reserved (HyperTerminal)
    ZMDM_VIRUS      = 0x16 // Error due to virus (HyperTerminal)
    ZMDM_REFUSE     = 0x17 // File refused, no reason given (HyperTerminal)
    ZMDM_OLDER      = 0x18 // File refused — older than existing (HyperTerminal)
    ZMDM_INUSE      = 0x19 // File is currently in use (HyperTerminal)
    ZMDM_CARRIER    = 0x1A // Lost carrier (HyperTerminal)
)

// Auto-download trigger string sent before ZRQINIT to activate terminal receivers.
// Terminal emulators (minicom, Tera Term, etc.) watch for "rz\r" to auto-start receive.
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
    ZMMASK   = 0x1f // Mask for management option bits
    ZMNEWL   = 1    // Transfer if newer or longer
    ZMCRC    = 2    // Transfer if different CRC
    ZMAPND   = 3    // Append to existing
    ZMCLOB   = 4    // Replace existing (clobber)
    ZMDIFF   = 5    // Transfer if different date/length
    ZMPROT   = 6    // Protect — only if absent
    ZMNEW    = 7    // Transfer if newer
    ZMCHNG   = 8    // Change filename if destination exists
    ZMSKNOLOC = 0x80 // Skip file if not present at receiver (bit flag, OR'd with above)
)

// ZSINIT flags (ZF0)
const (
    TESCCTL = 0x40 // Transmitter expects ctl chars escaped
    TESC8   = 0x80 // Transmitter expects 8th bit escaped
)

// Attention string meta-characters (inside AttnSequence)
const (
    AttnBreak = 0xDD // \335 octal — send break signal to remote
    AttnPause = 0xDE // \336 octal — pause one second
)
```

### 2. CRC (`crc.go`)

**CRITICAL**: ZMODEM CRC-16 requires a specific finalization step — two zero bytes must be fed through the CRC after the data. This is per lrzsz `zm.c` line 349: `crc = updcrc(0, updcrc(0, crc))`. Without this, ALL CRC-16 frames will fail verification against lrzsz and every other implementation.

```go
import (
    "hash/crc32"
    "github.com/sigurn/crc16"
)

var crc16Table = crc16.MakeTable(crc16.CRC16_XMODEM)

// crc16Calc computes ZMODEM CRC-16 with proper finalization.
// ZMODEM CRC-16 requires feeding two zero bytes through the CRC after the data.
// This is the "self-checking" property: when the receiver feeds data+CRC through
// the same algorithm, a correct frame yields CRC == 0.
func crc16Calc(data []byte) uint16 {
    crc := crc16.Update(0, data, crc16Table)
    crc = crc16.Update(crc, []byte{0, 0}, crc16Table)
    return crc
}

// crc16Verify checks if data (including the 2 CRC bytes) verifies correctly.
// After feeding data+CRC through the algorithm, result should be 0.
func crc16Verify(dataWithCRC []byte) bool {
    return crc16.Checksum(dataWithCRC, crc16Table) == 0
}

// CRC-32 uses the standard IEEE polynomial (same as ZMODEM spec).
// Init: 0xFFFFFFFF, finalize with XOR 0xFFFFFFFF (done internally by stdlib).
// Verification magic: when receiver feeds data+CRC32 through, result == 0x2144DF1C.
// lrzsz checks raw CRC against 0xDEBB20E3 (no final XOR). Go's ChecksumIEEE applies
// final XOR (^0xFFFFFFFF), so the magic becomes 0xDEBB20E3 ^ 0xFFFFFFFF = 0x2144DF1C.
func crc32Calc(data []byte) uint32 {
    return crc32.ChecksumIEEE(data)
}

const crc32VerifyMagic = 0x2144DF1C

func crc32Verify(dataWithCRC []byte) bool {
    return crc32.ChecksumIEEE(dataWithCRC) == crc32VerifyMagic
}
```

### 3. ZDLE Escape Engine (`escape.go`)

Table-driven like bforce's `zsendline()`. Must track "last sent byte" for CR-@-CR protection.

```go
// escapeTable[c] determines how to handle byte c:
//   0: send directly
//   1: must escape (ZDLE + c^0x40)
//   2: escape only if preceded by '@' (for CR protection)
var escapeTable [256]byte

func initEscapeTable(escapeAll bool) {
    // Always escape: ZDLE(0x18), DLE(0x10), XON(0x11), XOFF(0x13)
    // and their high-bit variants (0x90, 0x91, 0x93)
    // If escapeAll: escape all chars with bits 5+6 both zero (control chars)
    // Mode 2: CR(0x0d) and 0x8d — only if preceded by '@'(0x40) or 0xc0
}

// escapeWriter wraps a bufio.Writer and tracks lastSent for @-CR protection
type escapeWriter struct {
    w        *bufio.Writer
    table    *[256]byte
    lastSent byte
}

func (ew *escapeWriter) WriteByte(b byte) error { ... }
func (ew *escapeWriter) WriteEscaped(data []byte) error { ... }
func (ew *escapeWriter) Flush() error { return ew.w.Flush() }

func unescapeBytes(src []byte) ([]byte, error) { ... }

// zdlRead reads one unescaped byte from the transport,
// handling ZDLE sequences, ZRUB0/ZRUB1, and stripping XON/XOFF.
//
// ZDLE noise handling: If ZDLE is followed by a raw control character that
// doesn't match any valid escape sequence (not in 0x40-0x7F range after XOR),
// silently discard both bytes. This handles line noise that corrupts a ZDLE
// sequence. "Lenient on receive, strict on transmit."
func (r *transportReader) zdlRead() (byte, error) { ... }
```

### 4. Frame Header (`frame.go`)

Three frame formats: HEX, ZBIN (CRC-16), ZBIN32 (CRC-32).

```go
type Header struct {
    Encoding  byte    // ZBIN, ZHEX, or ZBIN32
    Type      byte    // Frame type (ZRQINIT, ZRINIT, etc.)
    Data      [4]byte // 4 bytes of position/flags
}

// Position returns header data as a 32-bit file offset (little-endian).
// Data[0] is LSB (P0), Data[3] is MSB (P3).
func (h *Header) Position() int64

// SetPosition sets header data from a file offset (little-endian).
func (h *Header) SetPosition(pos int64)

// Flags returns ZF0..ZF3 (same bytes as Data but indexed inversely).
// IMPORTANT: flags and position use OPPOSITE byte orders in the same 4 bytes!
//   Flags:    TYPE ZF3 ZF2 ZF1 ZF0  (ZF0 = Data[3], most significant position)
//   Position: TYPE P0  P1  P2  P3   (P0 = Data[0], least significant position)
// This is per spec: "Beware of the catch; flags and numbers are indexed the
// other way around!"
func (h *Header) ZF0() byte { return h.Data[3] }
func (h *Header) ZF1() byte { return h.Data[2] }
func (h *Header) ZF2() byte { return h.Data[1] }
func (h *Header) ZF3() byte { return h.Data[0] }

func (h *Header) SetZF0(v byte) { h.Data[3] = v }
func (h *Header) SetZF1(v byte) { h.Data[2] = v }
func (h *Header) SetZF2(v byte) { h.Data[1] = v }
func (h *Header) SetZF3(v byte) { h.Data[0] = v }

// Send methods
func (s *Session) sendBinHeader(hdr Header) error    // ZBIN or ZBIN32
func (s *Session) sendHexHeader(hdr Header) error     // ZHEX

// Receive method — auto-detects HEX/ZBIN/ZBIN32
// Also recognizes ZBINR32, ZVBIN, ZVHEX, ZVBIN32, ZVBINR32 and returns
// a clean error ("unsupported frame encoding") rather than corrupting state.
func (s *Session) recvHeader() (Header, error)
```

**HEX header format** (per spec):
```
ZPAD ZPAD ZDLE ZHEX <type-hex> <data[0]-hex> ... <data[3]-hex> <crc-hex> CR LF [XON]
```
- All values sent as 2 **lowercase** hex digits — uppercase MUST NOT be used as it
  false-triggers XMODEM/YMODEM receivers watching for 'C' or 'G' characters
- XON appended except for ZACK and ZFIN
- Receiver sends only HEX headers (always CRC-16, regardless of negotiated CRC mode)
- **Parity handling**: lrzsz sends LF as 0x8A (with parity bit set). Tera Term sends
  both CR and LF with parity bits (0x8D 0x8A). The reader MUST strip bit 7 (mask with
  0x7F) when reading HEX header CR/LF terminators, using `noxrd7()` like lrzsz does.

**ZBIN header format**:
```
ZPAD ZDLE ZBIN <type-escaped> <data[0]-escaped> ... <data[3]-escaped> <crc16-escaped>
```
- Preceded by `Znulls` null bytes when sending ZDATA (for modem turnaround)

**ZBIN32 header format**:
```
ZPAD ZDLE ZBIN32 <type-escaped> <data[0]-escaped> ... <data[3]-escaped> <crc32-escaped>
```

### 5. Data Subpackets (`subpacket.go`)

```go
// sendData sends a data subpacket with the given end type.
// Uses the session's escapeWriter (bufio.Writer backed) for performance.
// Flushes the write buffer at the end of the subpacket.
//
// IMPORTANT: Data subpackets (ZCRCE/ZCRCG/ZCRCQ/ZCRCW) are NOT frames/headers.
// They are subpacket terminators/footers within a ZDATA stream. This is the #1
// confusion among ZMODEM implementors (per Stack Overflow).
//
// CRC scope: The CRC covers the data bytes AND the end-type byte itself.
// The end-type byte is fed into the CRC before finalization, not after.
func (s *Session) sendData(data []byte, endType byte) error {
    // 1. For each byte: escape if needed via escapeWriter, update CRC
    // 2. Feed endType byte into CRC (CRITICAL: endType is part of CRC calculation)
    // 3. Finalize CRC (2-zero-byte for CRC-16, NOT+invert for CRC-32)
    // 4. Send ZDLE + endType
    // 5. Send CRC (2 bytes for CRC-16, 4 for CRC-32), each byte escaped
    // 6. If endType == ZCRCW: send XON (per lrzsz zm.c line 443)
    // 7. Flush the bufio.Writer
}

// recvData reads a data subpacket, returns data and end type.
// Per-byte timeout on ZDLE escape sequences prevents hanging on dead sender.
func (s *Session) recvData(maxLen int) ([]byte, byte, error) {
    // 1. Read bytes, unescaping ZDLE sequences
    // 2. On ZDLE + (ZCRCE|ZCRCG|ZCRCQ|ZCRCW): read CRC, verify
    // 3. Return data, end type, error
}
```

### 6. File Info (`fileinfo.go`)

Parse/marshal the ZFILE subpacket data per spec:

```
<filename>\0<size> <modtime> <mode> <serial> <files_remaining> <bytes_remaining>\0
```

- `filename`: null-terminated, lowercase, forward slashes only
- `size`: decimal string
- `modtime`: octal string (seconds since Unix epoch)
- `mode`: octal string (Unix file mode, 0 if non-Unix)
- `serial`: always 0
- `files_remaining`, `bytes_remaining`: decimal strings, estimates

All fields after filename are optional. Missing fields default to 0.

**Security**: The library does NOT sanitize filenames — that is the `FileHandler.AcceptFile`
implementation's responsibility. However, the library provides a helper:

```go
// SanitizeFilename returns a safe filename by stripping directory components
// and rejecting path traversal sequences. Returns filepath.Base(name).
func SanitizeFilename(name string) string
```

### 7. Sender State Machine (`sender.go`)

Explicit states following bforce's pattern:

```go
type senderState int
const (
    stxInit       senderState = iota // Send ZRQINIT, wait for ZRINIT
    stxSInit                          // Optional: send ZSINIT with Attn
    stxFileInfo                       // Send ZFILE + file metadata subpacket
    stxFileInfoAck                    // Wait for ZRPOS/ZSKIP/ZCRC
    stxData                           // Send ZDATA header + data subpackets
    stxDataDone                       // Data sent, check for errors
    stxEOF                            // Send ZEOF
    stxEOFAck                         // Wait for ZRINIT (next file) or error
    stxNextFile                       // Get next file from handler
    stxFin                            // Send ZFIN
    stxFinAck                         // Wait for ZFIN response, send OO
    stxDone                           // Session complete
)
```

**Reverse channel goroutine** (critical for streaming performance):

The sender spawns a goroutine that continuously reads from the transport and feeds
received headers into a `chan Header`. The main sender loop checks this channel
non-blocking (via `select` with `default`) after each data subpacket, like lrzsz's
`rdchk()` call. This avoids blocking the sender on reverse channel I/O.

```go
// Inside Send():
rxCh := make(chan Header, 4)
go s.reverseChannelReader(ctx, rxCh) // reads transport, parses headers, sends to rxCh
defer close(rxCh)
```

**Data transmission loop** (stxData):
1. Read block from file (up to MaxBlockSize)
2. Choose subpacket end type:
   - `ZCRCE` if EOF reached
   - `ZCRCW` if error count high or buffer sync needed
   - `ZCRCQ` if window management active
   - `ZCRCG` otherwise (continuous streaming)
3. Send subpacket via escapeWriter (buffered writes)
4. Check rxCh non-blocking for ZRPOS/ZACK
5. Handle ZRPOS: seek file (requires io.ReadSeeker), restart data frame
6. Handle ZSKIP: advance to next file
7. Call FileProgress periodically

**ZRPOS with non-seekable reader**:
If the `FileOffer.Reader` does not implement `io.ReadSeeker` and the receiver sends
ZRPOS with offset > 0, the sender sends ZSKIP to skip the file (cannot resume).

**Error recovery**:
- On ZRPOS: seek to indicated offset, send new ZDATA header, resume
- On ZNAK: resend last header
- On timeout: retry with ZCRCW (force ACK)
- Max retries before abort (configurable, default 10)

**Block size adaptation** (synthesized from lrzsz, Mystic, and Qodem):
- Start at 256 bytes (or 1024 for TCP/pipe transports where errors are rare)
- Track `goodBlocks` counter and `goodNeeded` threshold (initial: 8)
- After `goodBlocks >= goodNeeded`: double block size (up to MaxBlockSize)
- On ZRPOS error: cut block size to 1/4 (minimum 32 bytes), reset `goodBlocks=0`,
  double `goodNeeded` (cap at 16). This aggressive reduction + slow recovery gives
  the most reliable behavior on noisy links.
- Qodem uses a simpler variant: double after 8KB error-free, halve on error (min 32)
- **Window management**: Use `ZCRCQ` (request ACK) every N blocks:
  - Reliable link (no errors seen): every 32 blocks
  - Unreliable link (any error seen): every 4 blocks
  - The "unreliable" flag is one-way — once set, never goes back (Qodem pattern)

**Auto-download trigger**:
Before sending ZRQINIT, the sender should send the `"rz\r"` auto-download string.
Terminal emulators (minicom, Tera Term, HyperTerminal) watch for this to auto-start
the receiving program. This is especially important for BBS/terminal contexts.

**ZSINIT special handling**:
- When sending ZSINIT, the attention string data subpacket MUST have control characters
  escaped even if `EscapeAll` is not globally set. Temporarily enable escape-all for the
  ZSINIT data subpacket only (Qodem workaround).
- When CRC-32 is active, send ZSINIT as binary (not hex) because lrzsz always assumes
  CRC-32 for ZSINIT regardless of actual encoding. Sending hex ZSINIT with CRC-16 when
  CRC-32 is negotiated will fail against lrzsz.

**lrzsz interop notes**:
- lrzsz's `rz` sends ZRINIT repeatedly on a 10-second timer (for 40 seconds total
  before falling back to YMODEM). Sender must tolerate receiving multiple ZRINIT
  headers during handshake.
- lrzsz's `sz` sends ZRQINIT up to ~11 times before giving up. Receiver must respond
  to each.
- `sz` crashes on ZRPOS with offset > file size. Always validate: `offset <= fileSize`.
- If receiver sends "C", "G", or NAK instead of ZRINIT, this indicates an
  XMODEM/YMODEM-only receiver. Log and abort cleanly.

### 8. Receiver State Machine (`receiver.go`)

```go
type receiverState int
const (
    srxInit       receiverState = iota // Send ZRINIT, wait for ZFILE/ZSINIT
    srxSInit                            // Process ZSINIT, send ZACK
    srxFileWait                         // Wait for ZFILE
    srxFileAccept                       // Process file, send ZRPOS or ZSKIP
    srxData                             // Receive ZDATA + subpackets
    srxEOF                              // Process ZEOF, verify file
    srxNextFile                         // Wait for next ZFILE or ZFIN
    srxFin                              // Send ZFIN response
    srxDone                             // Session complete
)
```

**Data reception loop** (srxData):
1. Receive ZDATA header, verify position matches expected offset
2. Loop receiving subpackets:
   - On ZCRCG: write data, continue (no response)
   - On ZCRCQ: write data, send ZACK with current position
   - On ZCRCW: write data, send ZACK, wait for next frame
   - On ZCRCE: write data, frame done
3. On CRC error: **purge transport buffers** (drain ~100ms), then send ZRPOS with last good offset
4. On ZEOF: **IGNORE if offset doesn't match received bytes** (spec revision 07-31-1987).
   Responding to a mismatched ZEOF with ZRPOS causes double-retransmission loops.
   Only accept ZEOF when offset matches, then close file and send ZRINIT for next file.
5. Call FileProgress periodically

**Consecutive error detection**:
Track consecutive errors outside of ZDATA state. After ~15 consecutive errors,
conclude the peer is not running ZMODEM and abort the session (Qodem pattern).

**lrzsz receiver interop**:
- Force hex encoding for ZRPOS — `sz` requires hex ZRPOS, ignores binary
- Force hex encoding for ZCRC — `sz` requires hex ZCRC
- This applies even when CRC-32 is negotiated (hex headers are always CRC-16)

**Resume support**:
- AcceptFile returns offset > 0 → send ZRPOS with that offset
- Sender seeks to offset and resumes

**File growing during transfer**:
Per spec: "A file may grow after transmission commences, and all the data will be sent."
The Size field in FileInfo is an estimate. The receiver must accept more data than Size
indicated without erroring. Actual file length is determined by the data transfer.

### 9. Transport Layer (`reader.go`, `writer.go`)

#### Transport Reader (`reader.go`)

```go
type transportReader struct {
    r       io.Reader
    buf     *bufio.Reader  // buffered reads to handle short reads
    timeout time.Duration
}

// ReadByte reads one raw byte with timeout.
// Handles transports that return short reads correctly.
func (tr *transportReader) ReadByte() (byte, error)

// ReadZDLE reads one ZDLE-decoded byte (handles escapes, strips XON/XOFF).
// Has per-byte timeout for the second byte of a ZDLE escape sequence,
// preventing infinite hang if sender dies mid-escape.
func (tr *transportReader) ReadZDLE() (byte, bool, error)
// Returns: (byte, isSpecial, error)
// isSpecial=true for ZCRCE/ZCRCG/ZCRCQ/ZCRCW

// ReadHex reads two hex digits and returns the byte value.
// Strips parity bit (mask 0x7F) per lrzsz noxrd7() convention.
func (tr *transportReader) ReadHex() (byte, error)

// ScanForPad scans input for ZPAD (frame start), discarding other data.
// Returns the encoding byte that follows (ZBIN, ZHEX, ZBIN32).
// Returns error for ZBINR32, ZVBIN, ZVHEX, etc. (unsupported encodings).
// Tracks garbage count and returns error if threshold exceeded.
func (tr *transportReader) ScanForPad() (byte, error)
```

#### Transport Writer (`writer.go`)

```go
type transportWriter struct {
    w   *bufio.Writer  // CRITICAL: buffer writes to avoid per-byte syscalls
    ew  *escapeWriter  // ZDLE escape layer on top of bufio.Writer
}

// Flush writes buffered data to the underlying transport.
// Called at frame and subpacket boundaries.
func (tw *transportWriter) Flush() error

// WriteRaw writes bytes directly without escaping (for ZPAD, ZDLE, encoding byte).
func (tw *transportWriter) WriteRaw(data []byte) error

// WriteEscaped writes bytes with ZDLE escaping.
func (tw *transportWriter) WriteEscaped(data []byte) error

// WriteHex writes a byte as two lowercase hex digits.
func (tw *transportWriter) WriteHex(b byte) error
```

**Performance note**: A `bufio.Writer` with at least 4096 byte buffer is essential.
Without buffering, sending a 1024-byte data subpacket would make ~1024 individual
`Write()` syscalls, devastating throughput. Flush at subpacket boundaries.

**Allocation note**: For hot paths (escape encoding in data subpackets), consider
encoding directly into the bufio.Writer rather than allocating intermediate `[]byte`
slices. This reduces GC pressure during large file transfers.

### 10. Session Abort

```go
// Abort sequence: 8x CAN + 10x BS (per spec)
var abortSequence = []byte{
    0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, 0x18, // 8x CAN
    0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, 0x08, // 10x BS
}

// Detection: 5 consecutive CAN characters in input stream
```

## Protocol Features Checklist

### Core Protocol (Must Have)
- [x] HEX frame headers (receiver always uses these, always CRC-16)
- [x] ZBIN frame headers with CRC-16 (with proper 2-zero-byte finalization!)
- [x] ZBIN32 frame headers with CRC-32
- [x] ZDLE escape encoding/decoding (table-driven, with lastSent tracking)
- [x] ZDLE noise tolerance — silently discard ZDLE + raw control char (not valid escape)
- [x] XON/XOFF stripping from incoming data
- [x] All 20 standard frame types recognized
- [x] HyperTerminal extended types (0x14-0x1A) recognized and logged gracefully
- [x] Unsupported frame encodings (ZBINR32, ZVBIN, etc.) rejected cleanly
- [x] Data subpackets with all 4 end types (ZCRCE, ZCRCG, ZCRCQ, ZCRCW)
- [x] Data subpacket CRC includes end-type byte in calculation
- [x] ZRUB0/ZRUB1 escape handling
- [x] CR-@-CR Telenet escape protection
- [x] HEX header parity bit stripping (0x8A/0x8D from lrzsz/Tera Term)
- [x] HEX digits lowercase only (uppercase false-triggers XMODEM/YMODEM)
- [x] `"rz\r"` auto-download trigger before ZRQINIT
- [x] Session startup (ZRQINIT → ZRINIT handshake)
- [x] File transfer (ZFILE → ZRPOS → ZDATA → ZEOF)
- [x] ZEOF offset validation — ignore ZEOF with wrong offset (1987 spec revision)
- [x] Session cleanup (ZFIN → ZFIN → OO)
- [x] Batch file transfer (multiple files per session)
- [x] ZSKIP: skip files without aborting session
- [x] ZRPOS: resume/crash recovery (with seekable reader check, validate offset ≤ file size)
- [x] Abort sequence (8x CAN + 10x BS)
- [x] 5x CAN detection for session abort
- [x] File info subpacket (name, size, modtime, mode, files remaining, bytes remaining)
- [x] Proper header data byte order (flags vs position — opposite indexing!)
- [x] XON after ZCRCW data subpackets
- [x] Znulls before ZDATA binary headers (configurable)
- [x] Buffer purge before ZRPOS in error recovery

### Streaming Modes (Must Have)
Per Forsberg spec Chapter 9, four distinct streaming modes exist:
- [x] **Full streaming with sampling** (ZCRCG + non-blocking reverse channel goroutine) — primary mode
- [x] **Full streaming with reverse interrupt** (Attn sequence to interrupt sender) — for systems that can't sample
- [x] **Full streaming with sliding window** (ZCRCQ to elicit ZACKs, measure window) — for buffering systems
- [x] **Segmented streaming** (ZCRCW for receivers without overlapped I/O, uses receiver buffer size from ZRINIT)

After ZRPOS recovery, the **next** data frame must use ZCRCW (not ZCRCG) followed by
a wait, to guarantee network buffer flushing before resuming streaming.

### Error Recovery (Must Have)
- [x] CRC error → purge transport buffer, then ZRPOS with last good offset
- [x] ZRPOS recovery → next data frame uses ZCRCW (force ACK, guarantee flush)
- [x] ZNAK → resend last header
- [x] Timeout handling (receiver-driven timing; configurable idle timeout)
- [x] Retry limits with configurable max attempts (default 10 for init, 25 for file transfer per Mystic)
- [x] Garbage counter (default 1200 bytes, like lrzsz)
- [x] Per-byte timeout in ZDLE escape decoding (prevent hang on dead sender)
- [x] Consecutive error detection (~15 errors outside ZDATA = abort, peer not ZMODEM)

### Advanced Features (Should Have)
- [x] ZSINIT: attention sequence negotiation (with meta-chars: \335=break, \336=pause)
- [x] ZSINIT: force control char escaping in data subpacket (lrzsz compat)
- [x] ZSINIT: force binary encoding when CRC-32 active (lrzsz compat)
- [x] ZCHALLENGE: security challenge (basic — note: "most simply defeated security system ever", per Synchronet)
- [x] ZCRC: file CRC request/response (byte count 0 = entire file, init CRC to 0xFFFFFFFF)
- [x] ZCRC-based resume verification: compare CRC before resume, rename file on mismatch (Qodem pattern)
- [x] ZFREECNT: free disk space query (respond with actual free space or 0xFFFFFFFF sentinel)
- [x] Control character escaping negotiation (ESCCTL/ESC8)
- [x] Receiver capability flags (CANFDX, CANOVIO, CANBRK, CANFC32)
- [x] CANFC32 caution — some senders (Telix) malfunction when receiver advertises it
- [x] ZFILE management options (ZMNEWL, ZMCRC, ZMCLOB, ZMPROT, ZMCHNG, ZMSKNOLOC, etc.)
- [x] ZFILE conversion options (ZCBIN, ZCNL, ZCRECOV — note: ZCBIN does NOT override ZCRESUM)
- [x] Block size adaptation with adaptive threshold (goodNeeded doubles on error)
- [x] Window management: 32-block ACK interval (reliable) / 4-block (unreliable)
- [x] Progress callback for UI applications
- [x] Filename sanitization helper (path traversal protection)
- [x] MaxFileSize limit (resource exhaustion protection)
- [x] Consecutive error limit (~15) = peer not ZMODEM, abort
- [x] lrzsz receiver interop: force hex ZRPOS and hex ZCRC
- [x] XMODEM/YMODEM fallback detection (receiver sends "C"/"G"/NAK instead of ZRINIT)

### Extensions (Nice to Have)
- [ ] ZCOMMAND: remote command execution (security risk — opt-in only, Qodem rejects it)
- [ ] ZedZap: 8K blocks (increase MaxBlockSize to 8192, common in FidoNet/WaZOO)
- [ ] DirZap: no escaping (for clean 8-bit channels)
- [ ] RLE compression (ZBINR32 frames)
- [ ] Variable-length headers (ZVBIN, ZVHEX, etc.)
- [ ] ZMSPARS: sparse file support (receiver seeks to ZDATA position instead of comparing)
- [ ] File collision renaming (.0000, .0001, etc.) as alternative to overwrite/skip

## Implementation Order

### Phase 1: Core Framing (foundation)
1. `constants.go` — all protocol constants including ZBINR32, ZVBIN/ZVHEX/etc., ZRESC, ZMSKNOLOC, ZMCHNG
2. `crc.go` — CRC-16 with 2-zero-byte finalization, CRC-32 with verify magic, **unit tests for CRC correctness against known lrzsz test vectors**
3. `escape.go` + `escape_test.go` — ZDLE encode/decode with round-trip tests, lastSent tracking for @-CR
4. `writer.go` — buffered transport writer (`bufio.Writer` backed), escape layer, flush at boundaries
5. `reader.go` — buffered transport reader, ZDLE reading, parity stripping, frame scanning, garbage counter, **reverse channel reader goroutine infrastructure**

### Phase 2: Protocol Messages
6. `zmodem.go` — Session struct, Config, public API types (needed by frame.go and sender/receiver)
7. `frame.go` + `frame_test.go` — HEX/ZBIN/ZBIN32 header marshal/unmarshal, unsupported encoding rejection
8. `subpacket.go` + `subpacket_test.go` — data subpacket send/receive with proper CRC
9. `fileinfo.go` + `fileinfo_test.go` — ZFILE metadata parse/marshal, SanitizeFilename helper

### Phase 3: State Machines + Early Testing
10. `sender.go` — sender state machine (12 states) with reverse channel goroutine
11. `receiver.go` — receiver state machine (9 states)
12. `loopback_test.go` — sender↔receiver through `io.Pipe()` — **start testing ASAP**, don't wait until Phase 4

### Phase 4: Interop Testing
13. `cmd/zmodem-test/main.go` — CLI tool for interop testing with `sz`/`rz` (lrzsz)
14. Integration tests against lrzsz: single file, batch, skip, resume, error recovery
15. Test against known edge cases (see Testing Strategy below)

### Phase 5: Advanced Features
16. ZSINIT/attention sequence
17. ZCRC file checksum
18. ZCHALLENGE
19. ZFREECNT
20. Management/conversion options in ZFILE
21. Block size adaptation algorithm

## Estimated Size

| Component | Lines (approx) |
|-----------|----------------|
| Constants (incl. HyperTerminal, attention meta-chars) | 120 |
| CRC wrappers + finalization | 50 |
| Escape engine (with lastSent, noise tolerance) | 150 |
| Transport reader (buffered, parity, garbage, ZDLE noise) | 220 |
| Transport writer (buffered, escape layer, lowercase hex) | 110 |
| Frame headers (with lrzsz interop workarounds) | 240 |
| Data subpackets (CRC includes end-type byte) | 150 |
| File info + sanitize + collision rename | 120 |
| Session/API + reverse channel goroutine | 160 |
| Sender state machine (with ZSINIT compat, auto-download) | 450 |
| Receiver state machine (with ZEOF validation, hex ZRPOS, consecutive error detection) | 400 |
| **Total protocol code** | **~2170** |
| Tests (incl. byte-trace methodology, expanded edge cases) | ~900 |
| CLI test harness | ~100 |
| **Grand total** | **~3170** |

## Key Design Decisions

### 1. Transport is `io.ReadWriter`, not `io.Writer` only

The old implementation used `io.Writer` with consumer callbacks for the reverse channel. This is awkward because the protocol needs bidirectional I/O. Using `io.ReadWriter` is cleaner — the transport can be a serial port, TCP connection, or anything bidirectional.

### 2. Blocking API with context

`Send()` and `Receive()` block until complete. Use `context.Context` for cancellation. This is simpler than the old goroutine-per-frame approach and avoids data races. Calling both `Send()` and `Receive()` on the same session is explicitly forbidden (ZMODEM is half-duplex).

### 3. FileHandler interface instead of callbacks

A single interface instead of separate callback functions. Cleaner, testable, mockable. Includes `FileProgress` for real-world UI needs.

### 4. No global state

All state lives in the `Session` struct. Multiple sessions can run concurrently (e.g., in a multi-line FidoNet mailer).

### 5. CRC mode negotiation

Default to CRC-16. Upgrade to CRC-32 only if both sides advertise CANFC32. The sender checks the receiver's ZRINIT flags.

### 6. Block size adaptation

Start at 256 bytes for low-speed or error-prone links, increase to MaxBlockSize as transfer progresses error-free. Reduce on errors. (Following lrzsz's `calc_blklen()` strategy.)

### 7. FileOffer.Reader as io.Reader, not io.ReadSeeker

The `Reader` field accepts `io.Reader` for maximum flexibility. If the reader also implements `io.ReadSeeker`, resume via ZRPOS is enabled. If not, ZRPOS with non-zero offset causes the file to be skipped. This allows sending data from non-seekable sources (pipes, network streams) while still supporting resume for regular files.

### 8. Lenient on receive, strict on transmit

Postel's Law applied to ZMODEM. All historic implementations converge on this principle:
- **Transmit**: always escape all required bytes, use lowercase hex, proper CRC, correct byte order
- **Receive**: tolerate uppercase hex, missing XON, parity bits on CR/LF, ZDLE + noise bytes,
  extra ZPAD characters (0, 1, or 2), HyperTerminal extended frame types, and partial/malformed
  fields in file info. Mystic BBS removed its strict ZDLE validation because real senders violate
  the spec. Qodem documents specific lrzsz bugs it works around.

### 9. Buffered writes are mandatory

All output goes through `bufio.Writer`. Without this, per-byte syscall overhead makes the protocol unusable. Flush at frame/subpacket boundaries.

## Compatibility Targets

- **lrzsz** (`sz`/`rz`): primary interop target
  - Sends HEX LF with parity bit (0x8A) — reader must strip
  - `rz` sends ZRINIT repeatedly on 10s timer (40s total) — sender must tolerate
  - `sz` sends ZRQINIT up to ~11 times — receiver must respond
  - `sz` crashes on ZRPOS > file size — validate before seeking
  - `sz` requires hex ZRPOS and hex ZCRC — never send binary
  - `rz` assumes CRC-32 for ZSINIT — send as binary when CRC-32 active
  - ZSINIT data must escape control chars regardless of global setting
- **HyperTerminal**: Windows terminal emulator
  - Extended frame types 0x14-0x1A (ZBADFMT, ZMDM_VIRUS, etc.)
  - Fixed 1024-byte data subpackets (no adaptation)
  - Aggressive error model (abort rather than ZRPOS recovery)
  - Does not send second ZRPOS on "Skip file" (unlike lrzsz)
- **bforce**: FidoNet mailer (ZedZap 8K blocks support)
- **qico**: FidoNet mailer (ZedZap/DirZap)
- **FTNd**: FidoNet mailer (RLE, variable headers)
- **Tera Term**: terminal emulator (sends 0x8D 0x8A as CR LF in HEX headers)
- **minicom**: terminal emulator
- **SyncTerm**: BBS terminal (ZCRC handling quirks noted by Mystic implementor)
- **Telix**: legacy terminal — sends 35-byte packets when receiver advertises CANFC32

## Testing Strategy

1. **Unit tests**: escape round-trip, frame round-trip, file info parsing, **CRC correctness against known lrzsz values**
2. **Loopback test**: sender↔receiver through `io.Pipe()` — verifies complete protocol flow. **Start in Phase 3, not Phase 4**.
3. **Interop tests**: against lrzsz `sz`/`rz` through PTY — verifies real-world compatibility
4. **Byte trace methodology** (from Stack Overflow): capture exact byte sequences from lrzsz transfers and compare against library output byte-by-byte. This is the most effective debugging technique.
5. **Edge cases**:
   - File containing only ZDLE bytes (worst-case escaping)
   - Zero-length file (ZFILE → ZRPOS(0) → ZDATA(0) → ZEOF(0) without data subpackets)
   - File with spaces in name
   - File growing during transfer (more data than Size indicated)
   - Resume from middle of file (seekable reader)
   - Resume attempt on non-seekable reader (should skip gracefully)
   - ZCRC-based resume verification (CRC match → resume, CRC mismatch → rename)
   - Batch with skip in the middle
   - 5x CAN abort during transfer
   - Timeout recovery
   - CRC-32 negotiation and transfer
   - Filename with `../` path traversal attempt
   - Transport returning short reads (1 byte at a time)
   - Sender dying mid-ZDLE escape sequence (per-byte timeout)
   - File exceeding MaxFileSize limit
   - Garbage exceeding threshold
   - Multiple ZRINIT from receiver during handshake (lrzsz behavior)
   - ZEOF with wrong offset (must be ignored, not answered with ZRPOS)
   - ZRPOS with offset > file size (must not crash, validate before seeking)
   - HyperTerminal extended frame types (0x14-0x1A) — handle gracefully
   - ZDLE followed by raw control char (noise handling — silently discard)
   - Hex header with uppercase digits (should still parse, but never send)
   - Data containing bytes 0x10, 0x11, 0x13, 0x18, 0x7F, 0x8D, 0x90, 0x91, 0x93, 0xFF (full escape coverage)
   - Consecutive errors outside ZDATA (~15) triggering "not ZMODEM" abort
   - ZSINIT with attention string containing meta-chars (\335 break, \336 pause)
   - Block size adaptation: verify reduction on error and recovery after clean transfer
