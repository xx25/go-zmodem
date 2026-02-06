package zmodem

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// marshalFileInfo encodes file metadata for a ZFILE data subpacket.
// Format: <filename>\0<size> <modtime> <mode> <serial> <files_remaining> <bytes_remaining>\0
func marshalFileInfo(offer *FileOffer, filesRemaining int, bytesRemaining int64) []byte {
	// Filename: lowercase, forward slashes only
	name := strings.ToLower(offer.Name)
	name = strings.ReplaceAll(name, "\\", "/")

	// Build the metadata string after the null
	var meta strings.Builder
	meta.WriteString(fmt.Sprintf("%d", offer.Size))

	if !offer.ModTime.IsZero() {
		meta.WriteString(fmt.Sprintf(" %o", offer.ModTime.Unix()))
	} else {
		meta.WriteString(" 0")
	}

	meta.WriteString(fmt.Sprintf(" %o", offer.Mode))
	meta.WriteString(" 0") // serial number, always 0

	if filesRemaining > 0 {
		meta.WriteString(fmt.Sprintf(" %d", filesRemaining))
		if bytesRemaining > 0 {
			meta.WriteString(fmt.Sprintf(" %d", bytesRemaining))
		}
	}

	// Result: name + NUL + metadata + NUL
	result := make([]byte, 0, len(name)+1+meta.Len()+1)
	result = append(result, []byte(name)...)
	result = append(result, 0)
	result = append(result, []byte(meta.String())...)
	result = append(result, 0)
	return result
}

// parseFileInfo parses a ZFILE data subpacket into FileInfo.
// Format: <filename>\0<size> <modtime> <mode> <serial> <files_remaining> <bytes_remaining>\0
// All fields after filename are optional.
func parseFileInfo(data []byte) (FileInfo, error) {
	var info FileInfo

	// Find the first NUL (end of filename)
	nullIdx := -1
	for i, b := range data {
		if b == 0 {
			nullIdx = i
			break
		}
	}
	if nullIdx < 0 {
		return info, fmt.Errorf("zmodem: file info missing null terminator")
	}

	info.Name = string(data[:nullIdx])

	// Parse space-separated fields after the filename NUL
	rest := data[nullIdx+1:]
	// Trim trailing NUL(s)
	for len(rest) > 0 && rest[len(rest)-1] == 0 {
		rest = rest[:len(rest)-1]
	}

	if len(rest) == 0 {
		return info, nil
	}

	fields := strings.Fields(string(rest))

	// Field 0: size (decimal)
	if len(fields) > 0 {
		size, err := strconv.ParseInt(fields[0], 10, 64)
		if err == nil {
			info.Size = size
		}
	}

	// Field 1: modtime (octal, seconds since Unix epoch)
	if len(fields) > 1 {
		modtime, err := strconv.ParseInt(fields[1], 8, 64)
		if err == nil && modtime > 0 {
			info.ModTime = time.Unix(modtime, 0)
		}
	}

	// Field 2: mode (octal)
	if len(fields) > 2 {
		mode, err := strconv.ParseUint(fields[2], 8, 32)
		if err == nil {
			info.Mode = uint32(mode)
		}
	}

	// Field 3: serial (ignored, always 0)

	// Field 4: files remaining (decimal)
	if len(fields) > 4 {
		fr, err := strconv.Atoi(fields[4])
		if err == nil {
			info.FilesRemaining = fr
		}
	}

	// Field 5: bytes remaining (decimal)
	if len(fields) > 5 {
		br, err := strconv.ParseInt(fields[5], 10, 64)
		if err == nil {
			info.BytesRemaining = br
		}
	}

	return info, nil
}

// SanitizeFilename returns a safe filename by stripping directory components.
// Rejects path traversal sequences. Returns filepath.Base(name).
func SanitizeFilename(name string) string {
	// filepath.Base handles "../" and returns the last element
	return filepath.Base(name)
}
