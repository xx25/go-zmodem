# Plan: Fix Real Issues in go-zmodem

## Context

Four confirmed issues need fixing:
1. `Config.RecvTimeout` is declared but never wired to any I/O
2. Sender doesn't read the reverse channel (ZRPOS/ZNAK) during streaming data
3. Unused scaffolding: dead variables and error sentinels
4. ZMODEM-NEW.md has a stale CRC-32 magic constant

## Execution Order

**Phase 1** — Cleanup (Issue 3 + Issue 4): remove dead code, fix stale doc
**Phase 2** — RecvTimeout (Issue 1): idle-timeout deadline in transportReader
**Phase 3** — Reverse channel (Issue 2): sender mid-stream sampling, window flow control
**Phase 4** — Tests and verification

---

## Phase 1: Cleanup

### 1a. Remove unused error sentinels — `reader.go`

Delete lines 14-15:
```go
errBadEscape      = errors.New("zmodem: invalid ZDLE escape sequence") //nolint:unused
errTimeout        = errors.New("zmodem: read timeout")               //nolint:unused
```

### 1b. Remove `lastHeader` — `sender.go`

The per-state ZNAK handling already transitions to the correct resend state; `lastHeader` is never read. Remove:
- Declaration (line 39)
- All 5 assignment sites (lines 62-63, 101-102, 158-159, 295-296, 337-338)
- `_ = lastHeader` (line 367)

### 1c. Remove `_ = unreliable` — `sender.go`

Keep the `unreliable` variable — it will be activated in Phase 3 for block size adaptation. Only remove line 368: `_ = unreliable`.

### 1d. Fix ZMODEM-NEW.md CRC-32 magic — `ZMODEM-NEW.md`

Lines 402 and 407: change `0xDEBB20E3` to `0x2144DF1C`. Add comment explaining the Go final-XOR difference (matching the comment already in `crc.go:100-101`).

### 1e. `go mod tidy`

Remove unused `github.com/creack/pty` from go.mod/go.sum.

---

## Phase 2: Wire RecvTimeout

### Approach

Idle-timeout semantic in `transportReader` (reader.go). Set `SetReadDeadline(now + timeout)` only when the bufio buffer is empty — i.e., right before a read that will actually hit the underlying transport. This:
- Behaves as an idle timeout (fires only when no data arrives within the window)
- Covers all read paths automatically (headers AND subpackets) without sprinkling deadline logic in frame.go or subpacket.go
- Has no per-byte overhead when data is flowing (reads from bufio buffer skip the syscall)

### 2a. Add deadline infrastructure to `transportReader` — `reader.go`

```go
type deadlineSetter interface {
    SetReadDeadline(time.Time) error
}
```

Add fields to `transportReader`:
```go
ds      deadlineSetter    // nil if transport lacks deadline support
timeout time.Duration     // idle timeout (from Config.RecvTimeout)
```

In `newTransportReader`, type-assert the raw `io.Reader` (before bufio wrap) and store:
```go
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
```

Update the call site in `zmodem.go:137` to pass `c.RecvTimeout`.

### 2b. Wire deadline into `readByte` and fix `readByteStrip` — `reader.go`

```go
func (tr *transportReader) readByte() (byte, error) {
    if tr.r.Buffered() == 0 && tr.ds != nil && tr.timeout > 0 {
        tr.ds.SetReadDeadline(time.Now().Add(tr.timeout))
    }
    return tr.r.ReadByte()
}
```

**Critical:** `readByteStrip()` currently calls `tr.r.ReadByte()` directly (reader.go:45), bypassing `readByte()`. Change it to call `tr.readByte()` instead:
```go
func (tr *transportReader) readByteStrip() (byte, error) {
    for {
        b, err := tr.readByte()  // was: tr.r.ReadByte()
        ...
    }
}
```

This is necessary because `readByteStrip()` → `zdlRead()` → `zdlEscape()` is the read path for all ZDLE-decoded data (subpackets, binary headers). Without this change, deadlines only cover hex header reads and `scanForPad`, not subpacket reads.

After this fix, `readByte()` is the single choke point for all **blocking** transport reads (headers and subpackets). No additional deadline wiring is needed in frame.go or subpacket.go (optional non-blocking reads are handled in 2e).

When `Buffered() > 0`, data is already available; no deadline needed. When `Buffered() == 0`, the next `ReadByte()` hits the transport; set deadline first. If the deadline fires, the read returns a timeout error. The stale deadline left behind doesn't matter: the next `readByte()` call either finds `Buffered() > 0` (skip) or sets a fresh deadline.

### 2c. Clear deadline on session exit — `zmodem.go`

After `Send()` or `Receive()` returns, the last `SetReadDeadline(now + timeout)` is still set on the transport. If the caller reuses the `net.Conn`, a stale deadline will break later reads once it expires. Add best-effort cleanup:

```go
func (s *Session) Send(ctx context.Context) error {
    if !s.acquire() {
        return errors.New("zmodem: session already active")
    }
    defer s.release()
    defer s.tr.clearDeadline()  // add this
    return s.runSender(ctx)
}
```

Same for `Receive()`. Add `clearDeadline()` to `transportReader`, guarded on `timeout > 0` so we don't clobber caller-managed deadlines when the feature is disabled:
```go
func (tr *transportReader) clearDeadline() {
    if tr.ds != nil && tr.timeout > 0 {
        _ = tr.ds.SetReadDeadline(time.Time{})
    }
}
```

**Document:** enabling `RecvTimeout` overwrites any caller-set read deadline on the transport while the session runs (cleared back to zero on exit). Callers who need their own deadline management should leave `RecvTimeout` at 0.

### 2d. Update `RecvTimeout` doc and constructor — `zmodem.go`

Update the field comment:
```go
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
```

### 2e. Avoid blocking direct `bufio.Reader` reads that bypass `readByte` — `frame.go`, `receiver.go`

There are two places that read from `s.tr.r` directly and can block without going through `readByte()`:

- `frame.go`: `Peek(1)` when consuming the optional XON after a hex header
- `receiver.go`: `io.ReadFull(s.tr.r, buf)` when reading the optional `"OO"` after ZFIN

Both are "optional best-effort" reads. They must **not** block waiting for bytes.

Fix strategy:
- Only attempt these reads if `s.tr.r.Buffered() > 0` (or `>= 2` for `"OO"`). Otherwise, skip them.

---

## Phase 3: Sender Reverse Channel

### Design

The sender samples the reverse channel **synchronously between subpackets** — no goroutines. Two mechanisms:

1. **peekForZPAD**: scan `bufio.Reader.Buffered()` bytes for ZPAD/CAN. No deadline probe — the idle timeout in Phase 2 handles populating the buffer during blocking reads. This is purely opportunistic (catches frames that happen to be buffered).

2. **ZCRCQ checkpoints + window flow control**:
   - Many receivers advertise buffer size `ZP0,ZP1 = 0,0` (nonstop I/O), so `remoteWindowSize` is often 0.
   - To discover reverse-channel events mid-stream (ZRPOS), the sender must still periodically force a read.

When the receiver advertises CANFDX, periodically emit ZCRCQ instead of ZCRCG to solicit ZACK responses (checkpoint). Track `(fileOffset - lastAckOffset)`. If the receiver also advertises a non-zero buffer size, additionally enforce window flow control (block when window is full).

Key insight: reads and writes are independent (full duplex). The sender can read a header from the reverse channel without closing its data frame. Only ZRPOS causes a frame restart.

### 3a. Parse receiver window size — `sender.go`, `zmodem.go`

Add `remoteWindowSize int` to `Session` struct. In `processZRINIT`, extract from ZRINIT `Data[0]` (ZP0) and `Data[1]` (ZP1):
```go
s.remoteWindowSize = int(hdr.Data[0]) | int(hdr.Data[1])<<8
```

### 3b. Add `peekForZPAD` — `reader.go`

Scan all buffered bytes, not just byte 0. This handles leading XON/XOFF noise that `readByteStrip()` normally filters but `Peek()` does not:

```go
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
```

No deadline probe. If nothing is buffered, return false — the sender continues streaming. The ZCRCQ checkpoints (step 3c) are the reliable mechanism.

### 3c. Rewrite `stxData` inner loop — `sender.go`

New local variables at stxData entry:
- `lastAckOffset int64` — last confirmed receiver position
- `subpacketCount int` — for ZCRCQ interval

Gate ZCRCQ on `CANFDX`: only emit ZCRCQ when `(s.remoteFlags & CANFDX) != 0`. Use it as a periodic checkpoint even if `remoteWindowSize == 0` (common).

Loop structure:
```
for each subpacket:
  1. ctx.Err() check
  2. peekForZPAD() → if true, read header via recvHeader():
     - ZRPOS → seek, shrink blockSize, set unreliable, re-enter stxData
     - ZACK  → update lastAckOffset, continue loop
     - errAbortReceived → return abort error
     - other error / unexpected frame → log, continue loop
	  3. Window check: if remoteWindowSize > 0 and (offset - lastAck) >= window:
	     - if CANFDX: send a zero-length ZCRCQ subpacket (`sendSubpacket(nil, ZCRCQ)`) to solicit ZACK
	     - else: send a zero-length ZCRCW subpacket (`sendSubpacket(nil, ZCRCW)`) to solicit ZACK, then restart with a new ZDATA header (ZCRCW ends the frame)
	     - recvHeader() blocking → expect ZACK or ZRPOS
	     - On timeout: increment retries; if retries >= MaxRetries, abort. Otherwise,
	       repeat (send zero-length subpacket, then recvHeader()). Reset retries to 0 on a
	       successful ZACK.
  4. Read file data
  5. Choose endType:
     - EOF → ZCRCE
     - CANFDX && every Nth subpacket (checkpoint interval, e.g. 8) → ZCRCQ
     - otherwise → ZCRCG
  6. sendSubpacket
  7. If ZCRCQ → recvHeader() for ZACK or ZRPOS (bounded by RecvTimeout).
     On timeout: apply the same retry strategy as step 3 (zero-length ZCRCQ, recvHeader).
  8. Block size adaptation (use unreliable for slower ramp: goodNeeded=16 vs 8)
  9. Progress callback
```

Note on step 2: abort is detected as `errAbortReceived` from `recvHeader()` → `scanForPad()` (5×CAN detection in reader.go:68-71), not as a ZCAN frame type. Handle via error check, not frame type switch.

When ZRPOS is received (steps 2, 3, or 7): seek file, set `blockSize = max(blockSize/4, 32)`, set `unreliable = true`, set `state = stxData`, break inner loop. The outer state machine re-enters stxData which re-sends the ZDATA header.

### 3d. Activate `unreliable` for block size ramp

When `unreliable` is true, use `goodNeeded = 16` instead of 8 for the block size doubling threshold. This makes the sender ramp up more slowly after errors.

---

## Phase 4: Verification

### Commands
```bash
go build ./...                # compile check
go vet ./...                  # lint
go test -run TestLoopback     # loopback tests (no external deps)
go test -run TestLrzsz        # integration tests (if rz/sz available)
go test -v ./...              # all tests verbose
```

### New test: mid-stream ZRPOS — `loopback_test.go`

Add `TestLoopbackMidStreamZRPOS`:
- Use a corrupting transport wrapper between sender and receiver that corrupts the CRC bytes at the end of one specific subpacket (deterministic CRC failure, minimal protocol desync). Identify the target by counting ZDLE+ZCRCG sequences in the sender→receiver stream and corrupting the CRC of the Nth one.
- The receiver detects a CRC error, purges, sends ZRPOS
- ZCRCQ checkpoints are emitted when CANFDX is set; optionally set the receiver's `WindowSize` config to a non-zero value to also exercise the "window full" path (zero-length ZCRCQ solicit + blocking read)
- Verify the complete file is received correctly despite the corruption
- Use the existing buffered channel transport (`newTestTransports()`) to avoid deadlocks. `net.Pipe()` is synchronous and unbuffered, and is a poor fit for mid-stream reverse-channel events.

### New test: RecvTimeout on deadline-capable transport — `*_test.go`

Add `TestRecvTimeoutDeadlineCapableTransport`:
- Use `net.Pipe()` (implements `SetReadDeadline`)
- Configure `RecvTimeout` to a small value (e.g., 50ms)
- Start a `Receive()` (or `Send()` waiting for headers) without the peer sending data
- Assert it returns with a timeout-like error within a bounded time

This tests Phase 2 without mixing it with the reverse-channel behavior test.

### What to verify
- **Existing loopback tests pass**: channel-based transports have `ds == nil`, so `peekForZPAD` only checks `Buffered()` (returns false during normal streaming → same behavior as before). Idle timeout is a no-op.
- **lrzsz tests pass**: TCP transports support deadlines; RecvTimeout idle timeout is now active when configured (>0); ZCRCQ checkpoints are emitted when the peer advertises CANFDX; window enforcement is only active if peer sets non-zero ZRINIT buffer size (ZP0/ZP1)
- **New ZRPOS test passes**: proves reverse channel actually works mid-stream
- **New RecvTimeout test passes**: proves deadlines are wired and cleaned up correctly

### Files modified
| File | Changes |
|------|---------|
| `reader.go` | Remove dead errors, add `deadlineSetter`/`ds`/`timeout`/`clearDeadline`, wire idle deadline into `readByte`, route `readByteStrip` through `readByte`, add `peekForZPAD` |
| `sender.go` | Remove `lastHeader`, rewrite `stxData` loop with reverse channel + ZCRCQ + window, activate `unreliable`, update `processZRINIT` |
| `zmodem.go` | Add `remoteWindowSize` to Session, update RecvTimeout doc, pass timeout to `newTransportReader`, add `clearDeadline` defer in `Send`/`Receive` |
| `loopback_test.go` | Add `TestLoopbackMidStreamZRPOS` using `newTestTransports()` buffered channel transport + corruption wrapper |
| `frame.go` | Make optional XON consumption non-blocking (`Buffered() > 0` guard) |
| `receiver.go` | Make optional `"OO"` read non-blocking (`Buffered() >= 2` guard) |
| `*_test.go` | Add `TestRecvTimeoutDeadlineCapableTransport` using `net.Pipe()` |
| `frame_test.go` | Update 4 `newTransportReader` call sites (add timeout param) |
| `subpacket_test.go` | Update 4 `newTransportReader` call sites (add timeout param) |
| `ZMODEM-NEW.md` | Fix CRC-32 magic constant |
| `go.mod` / `go.sum` | `go mod tidy` |
