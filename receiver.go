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

// runReceiver implements the receiver state machine.
func (s *Session) runReceiver(ctx context.Context) error {
	state := srxInit
	var (
		curInfo        FileInfo
		curWriter      io.WriteCloser
		fileOffset     int64
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
				// Send ZNAK
				if err := s.sendHexHeader(makeHeader(ZNAK)); err != nil {
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
				if retries > 25 { // higher limit for file transfer per Mystic
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
				if dataPos != fileOffset {
					s.logger.Warn("ZDATA position mismatch", "expected", fileOffset, "got", dataPos)
					// Send ZRPOS to correct
					s.tr.purge()
					if err := s.sendHexHeader(makePosHeader(ZRPOS, fileOffset)); err != nil {
						return err
					}
					continue
				}

				// Receive data subpackets
				if err := s.receiveDataSubpackets(ctx, curWriter, &curInfo, &fileOffset, &bytesReceived, &retries); err != nil {
					if err == errEOFReceived {
						state = srxEOF
						continue
					}
					// CRC error or other: purge and ZRPOS
					s.logger.Debug("data error, sending ZRPOS", "err", err, "offset", fileOffset)
					s.tr.purge()
					retries++
					if retries > 25 {
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
func (s *Session) receiveDataSubpackets(ctx context.Context, w io.Writer, info *FileInfo,
	offset *int64, received *int64, retries *int) error {

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		data, endType, err := s.recvSubpacket(s.cfg.MaxBlockSize + 256)
		if err != nil {
			return err
		}

		// Write data
		if len(data) > 0 {
			if _, err := w.Write(data); err != nil {
				return fmt.Errorf("zmodem: file write error: %w", err)
			}
			*offset += int64(len(data))
			*received = *offset
			*retries = 0 // successful data resets retry count

			// Progress callback
			s.handler.FileProgress(*info, *received)
		}

		switch endType {
		case ZCRCG:
			// Continue — no response needed
			continue

		case ZCRCQ:
			// Send ZACK with current position
			if err := s.sendHexHeader(makePosHeader(ZACK, *offset)); err != nil {
				return err
			}
			continue

		case ZCRCW:
			// Send ZACK, then wait for next frame
			if err := s.sendHexHeader(makePosHeader(ZACK, *offset)); err != nil {
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
