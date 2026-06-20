# zmodem

Pure Go implementation of the ZMODEM file transfer protocol.

This is a library package — there is no CLI. Users import the `zmodem` package and provide a `FileHandler` interface implementation to drive file transfers over any `io.ReadWriter` transport (TCP sockets, serial ports, SSH channels, etc.).

## Features

- Full ZMODEM batch send and receive
- CRC-16 and CRC-32 support with automatic negotiation
- Streaming and windowed transfer modes
- Resume (crash recovery) via ZRPOS when the reader implements `io.ReadSeeker`
- Adaptive block sizing (256 up to 8192 bytes)
- XON/XOFF stripping, control character escaping
- Tested against lrzsz (`rz`/`sz`) for interoperability

## Install

```
go get github.com/xx25/go-zmodem
```

## Usage

### Sending files

```go
package main

import (
	"context"
	"io"
	"log"
	"net"
	"os"

	"github.com/xx25/go-zmodem"
)

// sender implements zmodem.FileHandler for sending files.
type sender struct {
	files []*zmodem.FileOffer
	idx   int
}

func (s *sender) NextFile() *zmodem.FileOffer {
	if s.idx >= len(s.files) {
		return nil
	}
	f := s.files[s.idx]
	s.idx++
	return f
}

func (s *sender) AcceptFile(info zmodem.FileInfo) (io.WriteCloser, int64, error) {
	return nil, 0, nil // not used when sending
}

func (s *sender) FileProgress(info zmodem.FileInfo, bytesTransferred int64) {
	log.Printf("sending %s: %d bytes", info.Name, bytesTransferred)
}

func (s *sender) FileCompleted(info zmodem.FileInfo, bytesTransferred int64, err error) {
	if err != nil {
		log.Printf("failed %s: %v", info.Name, err)
	} else {
		log.Printf("sent %s: %d bytes", info.Name, bytesTransferred)
	}
}

func main() {
	conn, err := net.Dial("tcp", "localhost:9000")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	f, err := os.Open("document.txt")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	fi, _ := f.Stat()

	handler := &sender{
		files: []*zmodem.FileOffer{{
			Name:    fi.Name(),
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
			Reader:  f, // implements io.ReadSeeker, so resume works
		}},
	}

	sess := zmodem.NewSession(conn, handler, &zmodem.Config{
		Use32BitCRC:  true,
		MaxBlockSize: 8192,
	})

	if err := sess.Send(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

### Receiving files

```go
package main

import (
	"context"
	"io"
	"log"
	"net"
	"os"

	"github.com/xx25/go-zmodem"
)

// receiver implements zmodem.FileHandler for receiving files.
type receiver struct{}

func (r *receiver) NextFile() *zmodem.FileOffer { return nil } // not used when receiving

func (r *receiver) AcceptFile(info zmodem.FileInfo) (io.WriteCloser, int64, error) {
	// IMPORTANT: always sanitize the filename to prevent path traversal.
	safeName := zmodem.SanitizeFilename(info.Name)

	f, err := os.Create(safeName)
	if err != nil {
		return nil, 0, err
	}
	return f, 0, nil // offset 0 = receive from start
}

func (r *receiver) FileProgress(info zmodem.FileInfo, bytesTransferred int64) {
	log.Printf("receiving %s: %d / %d bytes", info.Name, bytesTransferred, info.Size)
}

func (r *receiver) FileCompleted(info zmodem.FileInfo, bytesTransferred int64, err error) {
	if err != nil {
		log.Printf("failed %s: %v", info.Name, err)
	} else {
		log.Printf("received %s: %d bytes", info.Name, bytesTransferred)
	}
}

func main() {
	ln, err := net.Listen("tcp", ":9000")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	conn, err := ln.Accept()
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	sess := zmodem.NewSession(conn, &receiver{}, &zmodem.Config{
		Use32BitCRC: true,
		MaxFileSize: 100 * 1024 * 1024, // reject files over 100 MB
	})

	if err := sess.Receive(context.Background()); err != nil {
		log.Fatal(err)
	}
}
```

### Skipping files

Return `zmodem.ErrSkip` from `AcceptFile` to skip a file:

```go
func (r *receiver) AcceptFile(info zmodem.FileInfo) (io.WriteCloser, int64, error) {
	if info.Size > 50*1024*1024 {
		return nil, 0, zmodem.ErrSkip
	}
	// ...
}
```

### Resuming transfers

Return a non-zero offset from `AcceptFile` to resume a partially received file. The sender's `FileOffer.Reader` must implement `io.ReadSeeker` for resume to work.

```go
func (r *receiver) AcceptFile(info zmodem.FileInfo) (io.WriteCloser, int64, error) {
	safeName := zmodem.SanitizeFilename(info.Name)

	fi, err := os.Stat(safeName)
	if err == nil && fi.Size() > 0 {
		// Resume: open for append, start at existing size
		f, err := os.OpenFile(safeName, os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, 0, err
		}
		return f, fi.Size(), nil
	}

	f, err := os.Create(safeName)
	if err != nil {
		return nil, 0, err
	}
	return f, 0, nil
}
```

## Configuration

`Config` controls session behavior:

| Field              | Default | Description                                         |
|--------------------|---------|-----------------------------------------------------|
| `MaxBlockSize`     | 1024    | Data subpacket size (max 8192)                      |
| `WindowSize`       | 0       | Streaming window size (0 = full streaming)          |
| `EscapeMode`       | `EscapeStandard` | ZDLE escaping mode: `EscapeStandard`, `EscapeAll`, `EscapeMinimal` |
| `Use32BitCRC`      | false   | Prefer CRC-32 when receiver supports it             |
| `AttnSequence`     | nil     | Attention string for interrupting sender (max 32 B) |
| `RecvTimeout`      | 10s     | Idle timeout for reads (0 = disabled)               |
| `MaxFileSize`      | 0       | Max accepted file size (0 = unlimited)              |
| `MaxRetries`       | 10      | Max retransmission attempts before abort            |
| `GarbageThreshold` | 1200    | Max garbage bytes before aborting                   |

Pass `nil` for `Config` to use defaults (10s recv timeout, CRC-16, 1024-byte blocks).

## Security

- **Path traversal**: Incoming filenames may contain `../`. The library does **not** sanitize automatically. Use `zmodem.SanitizeFilename()` in your `AcceptFile` implementation.
- **Remote commands**: `ZCOMMAND` frames are rejected.
- **File size limits**: Set `Config.MaxFileSize` to cap accepted file sizes.

## License

See [LICENSE](LICENSE) for details.
