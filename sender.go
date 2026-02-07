package zmodem

import (
	"context"
	"errors"
	"fmt"
	"io"
)

type senderState int

const (
	stxInit        senderState = iota // Send ZRQINIT, wait for ZRINIT
	stxSInit                          // Optional: send ZSINIT with Attn
	stxFileInfo                       // Send ZFILE + file metadata subpacket
	stxFileInfoAck                    // Wait for ZRPOS/ZSKIP/ZCRC
	stxData                           // Send ZDATA header + data subpackets
	stxEOF                            // Send ZEOF
	stxEOFAck                         // Wait for ZRINIT (next file) or error
	stxNextFile                       // Get next file from handler
	stxFin                            // Send ZFIN
	stxFinAck                         // Wait for ZFIN response, send OO
	stxDone                           // Session complete
)

// runSender implements the sender state machine.
func (s *Session) runSender(ctx context.Context) error {
	state := stxInit
	var (
		curOffer     *FileOffer
		curInfo      FileInfo
		fileOffset   int64
		bytesSent    int64
		retries      int
		blockSize    int
		goodBlocks   int
		goodNeeded   int
		unreliable   bool
		zcrcwNext    bool
		zcrcwRetries int
		filesLeft    int
		bytesLeft    int64
	)

	blockSize = 256
	goodNeeded = 8

	for state != stxDone {
		if err := ctx.Err(); err != nil {
			return err
		}

		switch state {
		case stxInit:
			// Send auto-download trigger + ZRQINIT
			if err := s.tw.writeRaw(AutoDownloadString); err != nil {
				return err
			}
			hdr := makeHeader(ZRQINIT)
			if err := s.sendHexHeader(hdr); err != nil {
				return err
			}

			// Wait for ZRINIT
			rxHdr, err := s.recvHeaderRetry(ctx, &retries)
			if err != nil {
				return err
			}

			switch rxHdr.Type {
			case ZRINIT:
				s.processZRINIT(rxHdr)
				if len(s.cfg.AttnSequence) > 0 {
					state = stxSInit
				} else {
					state = stxNextFile
				}
			case ZCHALLENGE:
				// Echo back the challenge value
				resp := makePosHeader(ZACK, rxHdr.Position())
				if err := s.sendHexHeader(resp); err != nil {
					return err
				}
				// Stay in stxInit to wait for ZRINIT
			default:
				return fmt.Errorf("zmodem: sender expected ZRINIT, got %s", frameTypeName(rxHdr.Type))
			}

		case stxSInit:
			// Send ZSINIT with attention sequence
			hdr := makeHeader(ZSINIT)
			if s.cfg.EscapeMode == EscapeAll {
				hdr.SetZF0(TESCCTL)
			}

			// ZSINIT must be binary when CRC-32 active (lrzsz compat)
			if err := s.sendBinHeader(hdr); err != nil {
				return err
			}

			// ZSINIT data must escape control chars even if not globally set
			oldMode := s.tw.escapeMode
			s.tw.setEscapeMode(EscapeAll)
			attn := s.cfg.AttnSequence
			if len(attn) > 32 {
				attn = attn[:32]
			}
			attn = append(attn, 0) // null-terminate
			if err := s.sendSubpacket(attn, ZCRCW); err != nil {
				s.tw.setEscapeMode(oldMode)
				return err
			}
			s.tw.setEscapeMode(oldMode)

			// Wait for ZACK
			rxHdr, err := s.recvHeaderRetry(ctx, &retries)
			if err != nil {
				return err
			}
			switch rxHdr.Type {
			case ZACK:
				state = stxNextFile
			case ZNAK:
				retries++
				// Retry ZSINIT (stay in stxSInit)
			default:
				return fmt.Errorf("zmodem: sender expected ZACK for ZSINIT, got %s", frameTypeName(rxHdr.Type))
			}

		case stxNextFile:
			curOffer = s.handler.NextFile()
			if curOffer == nil {
				state = stxFin
				continue
			}
			curInfo = FileInfo{
				Name:    curOffer.Name,
				Size:    curOffer.Size,
				ModTime: curOffer.ModTime,
				Mode:    curOffer.Mode,
			}
			fileOffset = 0
			bytesSent = 0
			retries = 0
			goodBlocks = 0
			zcrcwNext = false
			zcrcwRetries = 0
			state = stxFileInfo

		case stxFileInfo:
			hdr := makeHeader(ZFILE)
			hdr.SetZF0(ZCBIN) // binary transfer

			if err := s.sendBinHeader(hdr); err != nil {
				return err
			}

			// Send file metadata subpacket
			meta := marshalFileInfo(curOffer, filesLeft, bytesLeft)
			if err := s.sendSubpacket(meta, ZCRCW); err != nil {
				return err
			}
			state = stxFileInfoAck

		case stxFileInfoAck:
			rxHdr, err := s.recvHeaderRetry(ctx, &retries)
			if err != nil {
				return err
			}

			switch rxHdr.Type {
			case ZRPOS:
				fileOffset = rxHdr.Position()
				// Validate offset
				if curOffer.Size > 0 && fileOffset > curOffer.Size {
					fileOffset = 0
				}
				if fileOffset > 0 {
					if err := s.seekFile(curOffer, fileOffset); err != nil {
						s.logger.Warn("cannot seek for resume, skipping", "file", curOffer.Name, "err", err)
						skipHdr := makeHeader(ZSKIP)
						if err := s.sendHexHeader(skipHdr); err != nil {
							return err
						}
						s.handler.FileCompleted(curInfo, 0, errors.New("cannot resume: reader not seekable"))
						state = stxNextFile
						continue
					}
				}
				bytesSent = fileOffset
				state = stxData

			case ZSKIP:
				s.handler.FileCompleted(curInfo, 0, ErrSkip)
				state = stxNextFile

			case ZCRC:
				crcVal, err := s.computeFileCRC(curOffer, rxHdr.Position())
				if err != nil {
					return err
				}
				resp := makePosHeader(ZCRC, int64(crcVal))
				if err := s.sendHexHeader(resp); err != nil {
					return err
				}
				// Stay in stxFileInfoAck

			case ZRINIT:
				// Extra ZRINIT — receiver responded to our ZRQINIT.
				// Process flags and continue waiting.
				s.processZRINIT(rxHdr)

			case ZNAK:
				retries++
				state = stxFileInfo // resend

			default:
				return fmt.Errorf("zmodem: sender expected ZRPOS/ZSKIP, got %s", frameTypeName(rxHdr.Type))
			}

		case stxData:
			// Send ZDATA header with current offset
			dataHdr := makePosHeader(ZDATA, fileOffset)
			if err := s.sendBinHeaderWithZnulls(dataHdr); err != nil {
				return err
			}

			// Data transmission loop with reverse channel sampling
			buf := make([]byte, s.cfg.MaxBlockSize)
			lastAckOffset := fileOffset
			var subpacketCount int
			canFDX := (s.remoteFlags & CANFDX) != 0
			const zcrcqInterval = 8

			sendLoop := false // true means break inner loop
			for !sendLoop {
				if err := ctx.Err(); err != nil {
					return err
				}

				// Check reverse channel (opportunistic, non-blocking)
				if s.tr.peekForZPAD() {
					rxHdr, err := s.recvHeader()
					if err != nil {
						if err == errAbortReceived {
							return err
						}
						s.logger.Debug("reverse channel read error", "err", err)
					} else {
						switch rxHdr.Type {
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
							zcrcwNext = true
							zcrcwRetries = 0
							state = stxData
							sendLoop = true
							continue
						case ZACK:
							lastAckOffset = rxHdr.Position()
						default:
							s.logger.Debug("unexpected reverse channel frame", "type", frameTypeName(rxHdr.Type))
						}
					}
				}

				// Window flow control: block when window is full
				if s.remoteWindowSize > 0 && (fileOffset-lastAckOffset) >= int64(s.remoteWindowSize) {
					// ZCRCQ is only valid when receiver advertises CANFDX (spec).
					// Without CANFDX, fall back to ZCRCW (force response before next frame).
					windowEndType := byte(ZCRCQ)
					if !canFDX {
						windowEndType = ZCRCW
					}

					// Solicit ZACK/ZRPOS with a zero-length subpacket.
					if err := s.sendSubpacket(nil, windowEndType); err != nil {
						return err
					}
					windowRetries := 0
					for {
						rxHdr, err := s.recvHeader()
						if err != nil {
							windowRetries++
							if windowRetries >= s.cfg.MaxRetries {
								return fmt.Errorf("zmodem: window flow control timeout after %d retries", windowRetries)
							}
							// Resend zero-length subpacket.
							if err := s.sendSubpacket(nil, windowEndType); err != nil {
								return err
							}
							continue
						}
						switch rxHdr.Type {
						case ZACK:
							lastAckOffset = rxHdr.Position()
							if windowEndType == ZCRCW {
								// ZCRCW ends the current data frame. Restart with a new ZDATA header.
								state = stxData
								sendLoop = true
							}
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
							zcrcwNext = true
							zcrcwRetries = 0
							state = stxData
							sendLoop = true
						default:
							s.logger.Debug("unexpected frame in window wait", "type", frameTypeName(rxHdr.Type))
							if windowEndType == ZCRCW {
								// We already ended the frame; restart to re-sync the receiver.
								state = stxData
								sendLoop = true
							}
						}
						break
					}
					if sendLoop {
						continue
					}
					// If window is still full after ZACK, re-check at top of loop
					if (fileOffset - lastAckOffset) >= int64(s.remoteWindowSize) {
						continue
					}
				}

				// Read file data
				n, readErr := curOffer.Reader.Read(buf[:blockSize])
				if n > 0 {
					atEOF := readErr == io.EOF

					// Choose end type
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

					if err := s.sendSubpacket(buf[:n], endType); err != nil {
						return err
					}
					fileOffset += int64(n)
					bytesSent = fileOffset
					subpacketCount++
					goodBlocks++

					// If ZCRCW (post-ZRPOS flush), wait for ZACK then restart frame
					if endType == ZCRCW {
						for {
							rxHdr, err := s.recvHeader()
							if err != nil {
								if err == errAbortReceived {
									return err
								}
								zcrcwRetries++
								if zcrcwRetries >= s.cfg.MaxRetries {
									return fmt.Errorf("zmodem: ZCRCW flush timeout after %d retries: %w", zcrcwRetries, err)
								}
								// Keep waiting; ZCRCW already ended the frame.
								continue
							}
							switch rxHdr.Type {
							case ZACK:
								ackPos := rxHdr.Position()
								// Per spec: ignore ZACK with an address that disagrees with the sender.
								if ackPos != fileOffset {
									s.logger.Debug("ignoring ZACK after ZCRCW flush (offset mismatch)",
										"got", ackPos, "want", fileOffset)
									zcrcwRetries++
									if zcrcwRetries >= s.cfg.MaxRetries {
										return fmt.Errorf("zmodem: ZCRCW flush max retries exceeded (stale ZACKs)")
									}
									continue
								}
								lastAckOffset = ackPos
								zcrcwNext = false
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
								zcrcwNext = true
								zcrcwRetries = 0
							default:
								s.logger.Debug("unexpected ZCRCW response", "type", frameTypeName(rxHdr.Type))
								zcrcwRetries++
								if zcrcwRetries >= s.cfg.MaxRetries {
									return fmt.Errorf("zmodem: ZCRCW flush max retries exceeded (unexpected frames)")
								}
								continue
							}
							break
						}
						// ZCRCW ends the frame; restart with fresh ZDATA header
						state = stxData
						sendLoop = true
						continue
					}

					// If ZCRCQ, read ZACK/ZRPOS response (bounded by RecvTimeout)
					if endType == ZCRCQ {
						zcrcqRetries := 0
						for {
							rxHdr, err := s.recvHeader()
							if err != nil {
								zcrcqRetries++
								if zcrcqRetries >= s.cfg.MaxRetries {
									return fmt.Errorf("zmodem: ZCRCQ response timeout after %d retries", zcrcqRetries)
								}
								// Solicit again with zero-length ZCRCQ
								if err := s.sendSubpacket(nil, ZCRCQ); err != nil {
									return err
								}
								continue
							}
							switch rxHdr.Type {
							case ZACK:
								lastAckOffset = rxHdr.Position()
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
								zcrcwNext = true
								zcrcwRetries = 0
								state = stxData
								sendLoop = true
							default:
								s.logger.Debug("unexpected ZCRCQ response", "type", frameTypeName(rxHdr.Type))
							}
							break
						}
						if sendLoop {
							continue
						}
					}

					// Block size adaptation
					adaptNeeded := goodNeeded
					if unreliable {
						adaptNeeded = 16
					}
					if goodBlocks >= adaptNeeded && blockSize < s.cfg.MaxBlockSize {
						blockSize *= 2
						if blockSize > s.cfg.MaxBlockSize {
							blockSize = s.cfg.MaxBlockSize
						}
						goodBlocks = 0
					}

					// Progress callback
					s.handler.FileProgress(curInfo, bytesSent)

					if atEOF {
						state = stxEOF
						sendLoop = true
					}
				} else if readErr != nil {
					if readErr == io.EOF {
						// Close the data frame with an empty ZCRCE subpacket.
						// Read may return (0, io.EOF) separately from the last
						// data chunk — ZMODEM spec requires ZCRCE before ZEOF.
						if err := s.sendSubpacket(nil, ZCRCE); err != nil {
							return err
						}
						state = stxEOF
					} else {
						return fmt.Errorf("zmodem: file read error: %w", readErr)
					}
					sendLoop = true
				}
			}

		case stxEOF:
			hdr := makePosHeader(ZEOF, fileOffset)
			if err := s.sendHexHeader(hdr); err != nil {
				return err
			}
			state = stxEOFAck

		case stxEOFAck:
			rxHdr, err := s.recvHeaderRetry(ctx, &retries)
			if err != nil {
				return err
			}

			switch rxHdr.Type {
			case ZRINIT:
				// File accepted, move to next
				s.handler.FileCompleted(curInfo, bytesSent, nil)
				s.processZRINIT(rxHdr)
				state = stxNextFile
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
				zcrcwNext = true
				zcrcwRetries = 0
				state = stxData
			case ZNAK:
				retries++
				state = stxEOF
			case ZSKIP:
				s.handler.FileCompleted(curInfo, bytesSent, ErrSkip)
				state = stxNextFile
			default:
				return fmt.Errorf("zmodem: sender expected ZRINIT after ZEOF, got %s", frameTypeName(rxHdr.Type))
			}

		case stxFin:
			hdr := makeHeader(ZFIN)
			if err := s.sendHexHeader(hdr); err != nil {
				return err
			}
			state = stxFinAck

		case stxFinAck:
			rxHdr, err := s.recvHeaderRetry(ctx, &retries)
			if err != nil {
				// Timeout at ZFIN is acceptable — session is done
				state = stxDone
				continue
			}

			switch rxHdr.Type {
			case ZFIN:
				// Send "OO" per protocol
				if err := s.tw.writeRaw([]byte("OO")); err != nil {
					return err
				}
				if err := s.tw.Flush(); err != nil {
					return err
				}
				state = stxDone
			case ZNAK:
				retries++
				state = stxFin
			default:
				state = stxDone
			}
		}

	}

	return nil
}

// processZRINIT processes receiver's ZRINIT flags.
func (s *Session) processZRINIT(hdr Header) {
	s.remoteFlags = hdr.ZF0()

	// Receiver buffer size (ZP0 = Data[0], ZP1 = Data[1])
	s.remoteWindowSize = int(hdr.Data[0]) | int(hdr.Data[1])<<8

	// CRC-32 negotiation
	if s.cfg.Use32BitCRC && (s.remoteFlags&CANFC32) != 0 {
		s.useCRC32 = true
	}

	// Escape negotiation
	if (s.remoteFlags & ESCCTL) != 0 {
		s.remoteEscAll = true
		s.tw.setEscapeMode(EscapeAll)
	}
}

// recvHeaderRetry receives a header with retry logic.
func (s *Session) recvHeaderRetry(ctx context.Context, retries *int) (Header, error) {
	for {
		if *retries >= s.cfg.MaxRetries {
			return Header{}, fmt.Errorf("zmodem: max retries (%d) exceeded", s.cfg.MaxRetries)
		}
		if err := ctx.Err(); err != nil {
			return Header{}, err
		}

		hdr, err := s.recvHeader()
		if err != nil {
			*retries++
			if *retries >= s.cfg.MaxRetries {
				return Header{}, fmt.Errorf("zmodem: max retries exceeded: %w", err)
			}
			continue
		}
		return hdr, nil
	}
}

// seekFile seeks a FileOffer's reader to the given offset.
func (s *Session) seekFile(offer *FileOffer, offset int64) error {
	seeker, ok := offer.Reader.(io.ReadSeeker)
	if !ok {
		return fmt.Errorf("reader does not implement io.ReadSeeker")
	}
	_, err := seeker.Seek(offset, io.SeekStart)
	return err
}

// computeFileCRC computes the CRC-32 of a file up to byteCount bytes.
// byteCount == 0 means the entire file.
func (s *Session) computeFileCRC(offer *FileOffer, byteCount int64) (uint32, error) {
	seeker, ok := offer.Reader.(io.ReadSeeker)
	if !ok {
		return 0, fmt.Errorf("reader does not implement io.ReadSeeker for ZCRC")
	}

	curPos, err := seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}

	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}

	var crc uint32
	buf := make([]byte, 8192)
	var totalRead int64

	for {
		toRead := int64(len(buf))
		if byteCount > 0 && totalRead+toRead > byteCount {
			toRead = byteCount - totalRead
		}
		if toRead <= 0 {
			break
		}
		n, err := offer.Reader.Read(buf[:toRead])
		if n > 0 {
			crc = crc32Update(crc, buf[:n])
			totalRead += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}

	if _, err := seeker.Seek(curPos, io.SeekStart); err != nil {
		return 0, err
	}

	return crc, nil
}
