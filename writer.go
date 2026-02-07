package zmodem

import (
	"bufio"
	"io"
)

const writerBufSize = 4096

// transportWriter wraps an io.Writer with buffering and ZDLE escaping.
type transportWriter struct {
	w         *bufio.Writer
	table     [256]byte
	lastSent  byte
	escapeAll bool
}

func newTransportWriter(w io.Writer, escapeAll bool) *transportWriter {
	tw := &transportWriter{
		w:         bufio.NewWriterSize(w, writerBufSize),
		escapeAll: escapeAll,
	}
	tw.table = buildEscapeTable(escapeAll)
	return tw
}

// setEscapeAll changes the escape mode and rebuilds the table.
func (tw *transportWriter) setEscapeAll(escapeAll bool) {
	tw.escapeAll = escapeAll
	tw.table = buildEscapeTable(escapeAll)
}

// Flush writes buffered data to the underlying transport.
func (tw *transportWriter) Flush() error {
	return tw.w.Flush()
}

// writeRaw writes bytes directly without escaping.
func (tw *transportWriter) writeRaw(data []byte) error {
	_, err := tw.w.Write(data)
	if len(data) > 0 {
		tw.lastSent = data[len(data)-1]
	}
	return err
}

// writeByte writes a single raw byte.
func (tw *transportWriter) writeByte(b byte) error {
	err := tw.w.WriteByte(b)
	if err == nil {
		tw.lastSent = b
	}
	return err
}

// writeEscaped writes bytes with ZDLE escaping.
func (tw *transportWriter) writeEscaped(data []byte) error {
	for _, b := range data {
		if err := tw.writeEscapedByte(b); err != nil {
			return err
		}
	}
	return nil
}

// writeEscapedByte writes a single byte, escaping if needed.
func (tw *transportWriter) writeEscapedByte(b byte) error {
	if escapeRequired(&tw.table, b, tw.lastSent) {
		esc1, esc2 := escapeByte(b)
		if err := tw.w.WriteByte(esc1); err != nil {
			return err
		}
		tw.lastSent = esc2
		return tw.w.WriteByte(esc2)
	}
	tw.lastSent = b
	return tw.w.WriteByte(b)
}

// writeHex writes a byte as two lowercase hex digits.
func (tw *transportWriter) writeHex(b byte) error {
	const hexDigits = "0123456789abcdef"
	if err := tw.w.WriteByte(hexDigits[b>>4]); err != nil {
		return err
	}
	return tw.w.WriteByte(hexDigits[b&0x0f])
}
