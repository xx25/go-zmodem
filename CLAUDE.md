# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Pure Go ZMODEM file transfer protocol library (no CLI). Module: `github.com/xx25/go-zmodem`. Zero external dependencies — Go stdlib only.

## Build & Test Commands

```bash
go build ./...                    # Compile (library, no binary output)
go test ./...                     # Run all 59 tests
go test -v ./...                  # Verbose
go test -run TestLoopbackSingle   # Run a single test by name
go test -run TestLrzsz            # Run all lrzsz interop tests
go test -count=1 ./...            # Disable test caching
go vet ./...                      # Static analysis
```

lrzsz integration tests (`lrzsz_test.go`) require `rz` and `sz` binaries on PATH. They are skipped automatically if not found.

## Architecture

### Public API (zmodem.go)

Users implement `FileHandler` (4 methods: `NextFile`, `AcceptFile`, `FileProgress`, `FileCompleted`) and create a `Session` with any `io.ReadWriter` transport. Call `sess.Send(ctx)` or `sess.Receive(ctx)`.

Key types: `Session`, `FileHandler`, `FileOffer` (sender→handler), `FileInfo` (handler←receiver), `Config`.

### Protocol State Machines

**sender.go** — 9 states (`stxInit` → `stxDone`): sends ZRQINIT, negotiates with ZRINIT, sends ZFILE+metadata per file, streams ZDATA subpackets with adaptive block sizing and reverse channel sampling, sends ZEOF, terminates with ZFIN.

**receiver.go** — 8 states (`srxInit` → `srxDone`): sends ZRINIT, waits for ZFILE, calls `AcceptFile`, receives ZDATA subpackets, handles ZEOF/resume/skip, terminates with ZFIN.

### Wire Format Layer

- **frame.go** — Header encoding/decoding in 3 formats: HEX (always CRC-16), ZBIN (CRC-16), ZBIN32 (CRC-32). A `Header` has a type byte + 4 data bytes (often encoding a 32-bit file position).
- **subpacket.go** — Data subpacket send/receive. Each subpacket ends with `ZDLE + endType + CRC`. End types: ZCRCG (continue), ZCRCQ (query/ACK), ZCRCW (wait), ZCRCE (end frame).
- **reader.go** — Buffered transport reader with ZDLE decoding, optional XON/XOFF stripping (disabled in DirZap/`EscapeMinimal` mode), garbage byte tracking, and CAN-abort detection.
- **writer.go** — Buffered transport writer with ZDLE escaping.

### Supporting Files

- **crc.go** — CRC-16 (lrzsz non-standard formula) and CRC-32 (IEEE). The CRC-16 table and algorithm match lrzsz exactly, not the standard XMODEM CRC-16.
- **escape.go** — Builds escape tables per `EscapeMode`. `EscapeStandard` covers ZDLE/DLE/XON/XOFF/CR-after-@; `EscapeAll` adds all control chars (hostile transports); `EscapeMinimal` (DirZap) escapes only ZDLE. 0x7F and 0xFF are never escaped.
- **fileinfo.go** — Marshals/parses ZFILE metadata subpackets (filename, size, modtime, mode, files/bytes remaining).
- **constants.go** — Frame types, ZDLE escape values, capability flags.

### Test Structure

- **Unit tests** (crc, escape, fileinfo, frame, subpacket `_test.go`): isolated component tests.
- **loopback_test.go**: 14 sender↔receiver integration tests over in-memory pipes (single file, batch, skip, resume, CRC-32, windowing, DirZap, error recovery, etc.).
- **lrzsz_test.go**: 15 interop tests against real `rz`/`sz` binaries via PTY.

## Protocol Pitfalls (from past debugging)

These are non-obvious behaviors that caused real bugs — be aware when modifying protocol code:

- **CRC-16 is lrzsz-specific**, not standard XMODEM. The table and formula in `crc.go` must match lrzsz exactly.
- **CRC-32 subpacket CRC** covers data + endType byte together. Go's `crc32.Update(0, table, data)` already handles init/final XOR internally — do NOT pass `0xFFFFFFFF` as the initial value.
- **CAN == ZDLE == 0x18**, so abort detection must happen inside the ZDLE/escape code path in the reader, not as a separate byte check.
- **0x7F and 0xFF cannot be ZDLE-escaped** (XOR 0x40 produces values < 0x40 that lrzsz rejects). They must pass through unescaped.
- **Sender must emit an empty ZCRCE subpacket** before ZEOF when `io.Read` returns `(0, io.EOF)` separately from the last data read, to properly close the data frame.
- **Receiver must set `useCRC32`** when it sees a ZBIN32 header on ZSINIT/ZFILE/ZDATA frames — not just from config.
