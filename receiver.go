package zmodem

import (
	"context"
	"fmt"
	"io"
)

type receiverState int

const (
	srxInit       receiverState = iota // Send ZRINIT, wait for ZFILE/ZSINIT
	srxSInit                           // Process ZSINIT, send ZACK
	srxFileWait                        // Wait for ZFILE
	srxFileAccept                      // Process file, send ZRPOS or ZSKIP
	srxData                            // Receive ZDATA + subpackets
	srxEOF                             // Process ZEOF, verify file
	srxNextFile                        // Wait for next ZFILE or ZFIN
	srxFin                             // Send ZFIN response
	srxDone                            // Session complete
)

// dataRetryBudget is the maximum number of failed recvHeader attempts the
// data phase (srxData) tolerates before aborting "max retries exceeded during
// data transfer". Higher than the file-wait MaxRetries per Mystic, because a
// single mid-stream data error must be recoverable: each retry purges and
// re-sends ZRPOS, and the in-flight backlog the receiver drains while hunting
// for the peer's resync frame can span several of these attempts.
const dataRetryBudget = 25

// runReceiver implements the receiver state machine.
func (s *Session) runReceiver(ctx context.Context) error {
	state := srxInit
	var (
		curInfo        FileInfo
		curWriter      io.WriteCloser
		fileOffset     int64
		incomingPos    int64 // position of the incoming byte stream (see srxData)
		bytesReceived  int64
		retries        int
		consecutiveErr int // errors outside ZDATA
	)

	const maxConsecutiveErr = 15

	for state != srxDone {
		if err := ctx.Err(); err != nil {
			return err
		}

		switch state {
		case srxInit:
			if err := s.sendZRINIT(); err != nil {
				return err
			}
			state = srxFileWait

		case srxFileWait:
			hdr, err := s.recvHeader()
			if err != nil {
				consecutiveErr++
				if consecutiveErr >= maxConsecutiveErr {
					return fmt.Errorf("zmodem: %d consecutive errors, peer likely not ZMODEM", consecutiveErr)
				}
				retries++
				if retries >= s.cfg.MaxRetries {
					return fmt.Errorf("zmodem: max retries exceeded waiting for ZFILE")
				}
				// Re-prompt the sender with ZRINIT, not ZNAK. While waiting
				// for the first ZFILE we hold no accepted file, so the
				// keep-waiting / timeout response must be "I am ready to
				// receive, send it again" — which is exactly ZRINIT. The
				// historical WaZOO mailers (bforce, BinkleyTerm XE,
				// xenia-mailer) all resend their receive-init header here and
				// never use ZNAK as the wait response; some peers mirror an
				// inbound ZNAK rather than advancing, which deadlocks the
				// handshake. This covers every recvHeader failure in this arm
				// (read timeout, garbage overflow, and hex/binary header CRC
				// errors alike): with no file yet to negotiate against, a
				// single uniform ZRINIT re-prompt is the safe answer. The
				// MaxRetries bound and the consecutiveErr "peer likely not
				// ZMODEM" guard above still terminate a truly dead peer.
				if err := s.sendZRINIT(); err != nil {
					return err
				}
				continue
			}
			consecutiveErr = 0

			switch hdr.Type {
			case ZRQINIT:
				// Sender is still initializing — resend ZRINIT
				if err := s.sendZRINIT(); err != nil {
					return err
				}

			case ZSINIT:
				// Enable CRC-32 if sender used ZBIN32 encoding
				if hdr.Encoding == ZBIN32 {
					s.useCRC32 = true
				}
				// Sender wants to set attention string
				data, _, err := s.recvSubpacket(256)
				if err != nil {
					return fmt.Errorf("zmodem: ZSINIT data error: %w", err)
				}
				// Store attention string (strip trailing NUL)
				for len(data) > 0 && data[len(data)-1] == 0 {
					data = data[:len(data)-1]
				}
				s.attnSeq = data

				// Process ZSINIT flags
				if (hdr.ZF0() & TESCCTL) != 0 {
					s.tw.setEscapeMode(EscapeAll)
				}

				// Send ZACK
				if err := s.sendHexHeader(makePosHeader(ZACK, 0)); err != nil {
					return err
				}

			case ZFILE:
				// Enable CRC-32 if sender used ZBIN32 encoding
				if hdr.Encoding == ZBIN32 {
					s.useCRC32 = true
				}
				// Parse file metadata from data subpacket
				data, _, err := s.recvSubpacket(2048)
				if err != nil {
					return fmt.Errorf("zmodem: ZFILE data error: %w", err)
				}

				info, err := parseFileInfo(data)
				if err != nil {
					return fmt.Errorf("zmodem: parse file info: %w", err)
				}
				curInfo = info

				// Check MaxFileSize
				if s.cfg.MaxFileSize > 0 && curInfo.Size > s.cfg.MaxFileSize {
					s.logger.Warn("file exceeds MaxFileSize, skipping",
						"file", curInfo.Name, "size", curInfo.Size, "max", s.cfg.MaxFileSize)
					if err := s.sendHexHeader(makeHeader(ZSKIP)); err != nil {
						return err
					}
					continue
				}

				state = srxFileAccept

			case ZFIN:
				state = srxFin

			case ZCOMMAND:
				// Reject remote commands (security)
				s.logger.Warn("ZCOMMAND received and rejected")
				if err := s.sendHexHeader(makePosHeader(ZCOMPL, 0)); err != nil {
					return err
				}

			case ZFREECNT:
				// Report free space (0xFFFFFFFF = unknown/unlimited)
				if err := s.sendHexHeader(makePosHeader(ZACK, 0x7FFFFFFF)); err != nil {
					return err
				}

			default:
				s.logger.Warn("unexpected frame in file wait", "type", frameTypeName(hdr.Type))
				consecutiveErr++
				if consecutiveErr >= maxConsecutiveErr {
					return fmt.Errorf("zmodem: %d consecutive errors, peer likely not ZMODEM", consecutiveErr)
				}
			}

		case srxFileAccept:
			// Ask application whether to accept
			writer, offset, err := s.handler.AcceptFile(curInfo)
			if err != nil {
				if err == ErrSkip {
					if err := s.sendHexHeader(makeHeader(ZSKIP)); err != nil {
						return err
					}
					s.handler.FileCompleted(curInfo, 0, ErrSkip)
					state = srxFileWait
					continue
				}
				return fmt.Errorf("zmodem: AcceptFile error: %w", err)
			}

			curWriter = writer
			fileOffset = offset
			bytesReceived = offset
			retries = 0

			// Send ZRPOS (always hex for lrzsz compat)
			if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
				return err
			}
			state = srxData

		case srxData:
			hdr, err := s.recvHeader()
			if err != nil {
				consecutiveErr++
				retries++
				if retries > dataRetryBudget {
					closeWriter(curWriter)
					s.handler.FileCompleted(curInfo, bytesReceived, fmt.Errorf("max retries exceeded"))
					return fmt.Errorf("zmodem: max retries exceeded during data transfer")
				}
				// Purge and send ZRPOS
				s.tr.purge()
				if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
					return err
				}
				continue
			}
			consecutiveErr = 0

			switch hdr.Type {
			case ZDATA:
				// Enable CRC-32 if sender used ZBIN32 encoding
				if hdr.Encoding == ZBIN32 {
					s.useCRC32 = true
				}
				dataPos := hdr.Position()
				switch {
				case dataPos > fileOffset:
					// The peer resumed AHEAD of the bytes we have written.
					// Our receive writer is append-only (AcceptFile hands back
					// a plain io.WriteCloser with no seek/truncate contract),
					// so we cannot leave a hole and fill it later. Re-ask the
					// peer to resume exactly at our write position.
					s.logger.Warn("ZDATA position ahead of write offset, re-requesting",
						"expected", fileOffset, "got", dataPos)
					s.tr.purge()
					if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
						return err
					}
					continue
				case dataPos < fileOffset:
					// The peer resumed BEHIND our write offset (it honoured an
					// earlier/lower ZRPOS, or retransmits a frame whose start is
					// below where we already are). Re-sending ZRPOS here is what
					// deadlocks the resume at large offsets: the peer keeps
					// answering from its frame boundary and we keep rejecting it.
					// Instead, accept the frame and discard the overlapping
					// [dataPos, fileOffset) bytes as receiveDataSubpackets
					// consumes them, writing only the tail once the incoming
					// stream catches up to fileOffset. The written fileOffset
					// stays monotonic — the overlap is dropped, never rewritten —
					// so this is safe against the append-only writer. incomingPos
					// is the separate cursor that tracks the incoming stream so
					// we know how much of each subpacket is duplicate.
					s.logger.Debug("ZDATA position behind write offset, discarding overlap",
						"writeOffset", fileOffset, "got", dataPos)
					incomingPos = dataPos
				default:
					incomingPos = fileOffset
				}

				// Receive data subpackets
				if err := s.receiveDataSubpackets(ctx, curWriter, &curInfo, &fileOffset, &incomingPos, &bytesReceived, &retries); err != nil {
					if err == errEOFReceived {
						state = srxEOF
						continue
					}
					// CRC error or other: purge and ZRPOS
					s.logger.Debug("data error, sending ZRPOS", "err", err, "offset", fileOffset)
					s.tr.purge()
					retries++
					if retries > dataRetryBudget {
						closeWriter(curWriter)
						s.handler.FileCompleted(curInfo, bytesReceived, fmt.Errorf("max retries exceeded"))
						return fmt.Errorf("zmodem: max retries exceeded during data transfer")
					}
					if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
						return err
					}
				}

			case ZEOF:
				// Validate offset
				eofPos := hdr.Position()
				if eofPos != fileOffset {
					// IGNORE mismatched ZEOF (spec revision 07-31-1987)
					s.logger.Warn("ZEOF offset mismatch, ignoring",
						"expected", fileOffset, "got", eofPos)
					continue
				}
				state = srxEOF

			case ZNAK:
				// Resend ZRPOS
				if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
					return err
				}

			case ZFILE:
				// Duplicate ZFILE — resend ZRPOS
				// This can happen if our ZRPOS was lost
				data, _, _ := s.recvSubpacket(2048) // consume the data subpacket
				_ = data
				if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
					return err
				}

			case ZFIN:
				// Session ending prematurely
				closeWriter(curWriter)
				s.handler.FileCompleted(curInfo, bytesReceived, fmt.Errorf("session ended prematurely"))
				state = srxFin

			case ZSKIP:
				// Sender cannot fulfil our ZRPOS (e.g. non-seekable reader).
				closeWriter(curWriter)
				curWriter = nil
				s.handler.FileCompleted(curInfo, bytesReceived, ErrSkip)
				state = srxFileWait

			default:
				s.logger.Warn("unexpected frame in data state", "type", frameTypeName(hdr.Type))
			}

		case srxEOF:
			closeWriter(curWriter)
			curWriter = nil
			s.handler.FileCompleted(curInfo, bytesReceived, nil)

			// Send ZRINIT for next file
			if err := s.sendZRINIT(); err != nil {
				return err
			}
			state = srxFileWait

		case srxFin:
			// Respond with ZFIN
			if err := s.sendHexHeader(makeHeader(ZFIN)); err != nil {
				return err
			}

			// Read "OO" (over and out) — best effort, only if already buffered
			if s.tr.r.Buffered() >= 2 {
				buf := make([]byte, 2)
				_, _ = io.ReadFull(s.tr.r, buf)
			}

			state = srxDone
		}
	}

	return nil
}

// errEOFReceived is a sentinel used internally to signal ZEOF during data reception.
var errEOFReceived = fmt.Errorf("EOF received")

// receiveDataSubpackets reads data subpackets until ZCRCE or error.
//
// offset is the append-only write position (advances only by bytes actually
// written). incomingPos is the position of the incoming byte stream and is
// always <= offset; it advances by the full length of every consumed subpacket,
// including bytes that fall inside an already-written overlap and are discarded.
// When the peer resumed below the write offset, the leading [incomingPos,
// offset) bytes of the stream are duplicates the append-only writer cannot
// rewrite, so they are dropped and only the tail beyond offset is written.
func (s *Session) receiveDataSubpackets(ctx context.Context, w io.Writer, info *FileInfo,
	offset *int64, incomingPos *int64, received *int64, retries *int) error {

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		data, endType, err := s.recvSubpacket(s.cfg.MaxBlockSize + 256)
		if err != nil {
			return err
		}

		// A valid subpacket (good CRC) is real progress toward the resume
		// point even when every byte falls inside the discarded overlap and
		// nothing is written. Reset the retry budget on the valid subpacket
		// itself, NOT gated on bytes written: when the peer resumed well below
		// our write offset the recovery streams many good-CRC-but-fully-
		// duplicate subpackets, and a write-gated reset would let the data
		// retry budget drain to exhaustion during the very catch-up it is
		// meant to enable.
		*retries = 0

		// Split the subpacket into the duplicate overlap (already written,
		// discard) and the new tail (write). incomingPos drives the discard,
		// not offset: offset does not move during a wholly-overlapping run, and
		// dataPos is only the frame's start.
		writeData := data
		if *incomingPos < *offset {
			overlap := *offset - *incomingPos
			if overlap >= int64(len(data)) {
				writeData = nil // wholly inside the overlap — drop it all
			} else {
				writeData = data[overlap:]
			}
		}
		*incomingPos += int64(len(data))

		// Write the new tail (if any)
		if len(writeData) > 0 {
			if _, err := w.Write(writeData); err != nil {
				return fmt.Errorf("zmodem: file write error: %w", err)
			}
			*offset += int64(len(writeData))
			*received = *offset

			// Progress callback
			s.handler.FileProgress(*info, *received)
		}

		// ZACK reports the incoming-stream position (= what the peer has sent),
		// which equals offset in the normal no-overlap case and trails it to
		// the peer's true position while catching up over an overlap.
		switch endType {
		case ZCRCG:
			// Continue — no response needed
			continue

		case ZCRCQ:
			// Send ZACK with current position
			if err := s.sendHexHeader(makePosHeader(ZACK, *incomingPos)); err != nil {
				return err
			}
			continue

		case ZCRCW:
			// Send ZACK, then wait for next frame
			if err := s.sendHexHeader(makePosHeader(ZACK, *incomingPos)); err != nil {
				return err
			}
			return nil // return to outer loop to read next header

		case ZCRCE:
			// End of frame — next should be ZEOF or ZDATA
			return nil
		}
	}
}

// sendZRINIT sends a ZRINIT header with our capabilities.
func (s *Session) sendZRINIT() error {
	hdr := makeHeader(ZRINIT)

	// Set capabilities
	caps := byte(CANFDX | CANOVIO)
	if s.cfg.Use32BitCRC {
		caps |= CANFC32
	}
	if s.cfg.EscapeMode == EscapeAll {
		caps |= ESCCTL
	}
	caps |= s.cfg.Capabilities
	hdr.SetZF0(caps)

	// ZF1: buffer size (0 = full streaming)
	if s.cfg.WindowSize > 0 {
		hdr.Data[0] = byte(s.cfg.WindowSize & 0xff)
		hdr.Data[1] = byte((s.cfg.WindowSize >> 8) & 0xff)
	}

	return s.sendHexHeader(hdr)
}

func closeWriter(w io.WriteCloser) {
	if w != nil {
		_ = w.Close()
	}
}
