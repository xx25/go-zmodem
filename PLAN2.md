# Plan: ZCRCW after ZRPOS recovery

## Context

The ZMODEM spec (zmodem.txt, "Full Streaming with Error Recovery") says:

> The next transmitted data frame should be a ZCRCW frame followed by a wait
> to guarantee complete flushing of the network's memory.

After ZRPOS, the receiver has purged its buffers and is waiting at the new
offset. On a buffered/long-delay link, the sender may have many subpackets
still in flight. Sending ZCRCW (which ends the frame and requires ZACK before
sending the next frame) forces a full round-trip, draining all stale data from
the pipe before resuming streaming.

Currently, after ZRPOS the sender re-enters `stxData`, sends a new ZDATA
header, and immediately resumes with ZCRCG/ZCRCQ subpackets — no flush.

## Approach

Add a `zcrcwNext bool` flag in `runSender` scope (next to the other recovery
variables). Add a small retry counter to bound waiting for the ZACK after the
flush.

Set `zcrcwNext` on *recovery* ZRPOS handlers that transition back to `stxData`
(mid-stream error recovery). Do **not** set it on the initial
ZRPOS received after ZFILE (normal start/resume handshake), because there is
no in-flight data to flush yet and it would force stop-and-wait at the start
of every file.

When the first subpacket after a recovery ZRPOS is
about to be sent, override the endType to ZCRCW. After sending the ZCRCW
subpacket, do a blocking `recvHeader()` for ZACK/ZRPOS (same pattern as existing
window-wait and ZCRCQ response code), then restart with a new ZDATA header.

This is a single-subpacket flush: send one ZCRCW data subpacket (with actual
file data, not zero-length), wait for ZACK, then re-enter stxData which sends
a fresh ZDATA header and resumes normal streaming.

## Changes — `sender.go`

### 1. Add `zcrcwNext` variables (alongside `blockSize`, `unreliable`, etc.)

At the top of `runSender`, next to the other recovery variables (~line 39):

```go
zcrcwNext  bool
zcrcwRetries int
```

Initialize to `false` (zero value).

Also reset it to `false` when starting a new file in `stxNextFile` (so the
flag cannot leak across files). Reset `zcrcwRetries = 0` there too.

### 2. Set `zcrcwNext = true` at every ZRPOS handler

Set `zcrcwNext = true` at each *recovery* ZRPOS handler that
transitions to `state = stxData`:

- **stxData reverse channel** (~line 257): add `zcrcwNext = true` before `state = stxData`
- **stxData window wait** (~line 313): add `zcrcwNext = true` before `state = stxData`
- **stxData ZCRCQ response** (~line 387): add `zcrcwNext = true` before `state = stxData`
- **stxEOFAck** (~line 463): add `zcrcwNext = true` before `state = stxData`

In each of these, also reset `zcrcwRetries = 0` when you set `zcrcwNext = true`.

### 3. Override endType in the subpacket send section

In the endType selection block (~line 339-348), add a ZCRCW override as the
highest-priority case:

```go
var endType byte
switch {
case zcrcwNext:
    endType = ZCRCW
case atEOF:
    endType = ZCRCE
case canFDX && subpacketCount > 0 && subpacketCount%zcrcqInterval == 0:
    endType = ZCRCQ
default:
    endType = ZCRCG
}
```

### 4. Handle the ZCRCW response

After `sendSubpacket`, the existing code handles ZCRCQ responses (~line
358-397). Add ZCRCW handling in the same area, right before the ZCRCQ block:

```go
if endType == ZCRCW {
    // ZCRCW ends the frame. Wait for ZACK, then restart with new ZDATA header.
    //
    // IMPORTANT: Do not resume streaming without completing the flush. If we
    // time out waiting for ZACK, keep waiting (bounded by MaxRetries) rather
    // than restarting and sending new data; restarting would defeat the whole
    // purpose of the round-trip drain.
    //
    // Keep zcrcwNext == true until we successfully complete the flush (ZACK
    // with matching offset). Ignore stale/mismatched ZACKs per spec.
    for {
        rxHdr, err := s.recvHeader()
        if err != nil {
            if err == errAbortReceived {
                return err
            }
            zcrcwRetries++
            if zcrcwRetries >= s.cfg.MaxRetries {
                return fmt.Errorf("zmodem: ZCRCW flush timeout after %d retries", zcrcwRetries)
            }
            continue
        }

        switch rxHdr.Type {
        case ZACK:
            ackPos := rxHdr.Position()
            if ackPos != fileOffset {
                // Per spec: ignore ZACK with an address that disagrees with the sender.
                zcrcwRetries++
                if zcrcwRetries >= s.cfg.MaxRetries {
                    return fmt.Errorf("zmodem: ZCRCW flush max retries exceeded (stale ZACKs)")
                }
                continue
            }
            lastAckOffset = ackPos
            zcrcwNext = false // flush complete
            zcrcwRetries = 0
        case ZRPOS:
            newPos := rxHdr.Position()
            if err := s.seekFile(curOffer, newPos); err != nil {
                return err
            }
            fileOffset = newPos
            bytesSent = newPos
            blockSize = max(blockSize/4, 32)
            goodBlocks = 0
            unreliable = true
            zcrcwNext = true // still need flush after recovery ZRPOS
            zcrcwRetries = 0
        default:
            // Unexpected frame after ZCRCW: ignore and keep waiting (bounded by MaxRetries).
            zcrcwRetries++
            if zcrcwRetries >= s.cfg.MaxRetries {
                return fmt.Errorf("zmodem: ZCRCW flush max retries exceeded (unexpected frames)")
            }
            continue
        }

        // ZCRCW ends the frame; restart with fresh ZDATA header
        state = stxData
        sendLoop = true
        break
    }

    if sendLoop {
        continue
    }
}
```

## Files modified

| File | Change |
|------|--------|
| `sender.go` | Add `zcrcwNext`/`zcrcwRetries`, set at recovery ZRPOS sites, reset per file, override endType, handle ZCRCW response |

## Verification

```bash
go build ./...
go vet ./...
# Optional:
# go test -run TestLoopback -v    # loopback tests (includes MidStreamZRPOS)
# go test -run TestLrzsz -v       # lrzsz integration (if rz/sz available)
```

The existing `TestLoopbackMidStreamZRPOS` test exercises the ZRPOS recovery
path — it should continue to pass (and now the recovery frame will use ZCRCW
instead of ZCRCG). No new test needed for this change since the behavior
difference (ZCRCW vs ZCRCG after ZRPOS) is only observable on high-latency
links; the existing test verifies the recovery path completes successfully.
