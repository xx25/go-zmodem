package zmodem

import (
	"testing"
	"time"
)

func TestMarshalParseFileInfoRoundTrip(t *testing.T) {
	offer := &FileOffer{
		Name:    "test.txt",
		Size:    12345,
		ModTime: time.Unix(1234567890, 0),
		Mode:    0644,
	}

	data := marshalFileInfo(offer, 3, 50000)

	info, err := parseFileInfo(data)
	if err != nil {
		t.Fatalf("parseFileInfo error: %v", err)
	}

	if info.Name != "test.txt" {
		t.Errorf("name = %q, want %q", info.Name, "test.txt")
	}
	if info.Size != 12345 {
		t.Errorf("size = %d, want %d", info.Size, 12345)
	}
	if info.ModTime.Unix() != 1234567890 {
		t.Errorf("modtime = %d, want %d", info.ModTime.Unix(), 1234567890)
	}
	if info.Mode != 0644 {
		t.Errorf("mode = 0%o, want 0644", info.Mode)
	}
	if info.FilesRemaining != 3 {
		t.Errorf("filesRemaining = %d, want 3", info.FilesRemaining)
	}
	if info.BytesRemaining != 50000 {
		t.Errorf("bytesRemaining = %d, want 50000", info.BytesRemaining)
	}
}

func TestParseFileInfoMinimal(t *testing.T) {
	// Just filename and null
	data := []byte("hello.bin\x00")

	info, err := parseFileInfo(data)
	if err != nil {
		t.Fatalf("parseFileInfo error: %v", err)
	}

	if info.Name != "hello.bin" {
		t.Errorf("name = %q, want %q", info.Name, "hello.bin")
	}
	if info.Size != 0 {
		t.Errorf("size = %d, want 0", info.Size)
	}
}

func TestParseFileInfoSizeOnly(t *testing.T) {
	data := []byte("file.dat\x0042000\x00")

	info, err := parseFileInfo(data)
	if err != nil {
		t.Fatalf("parseFileInfo error: %v", err)
	}

	if info.Name != "file.dat" {
		t.Errorf("name = %q, want %q", info.Name, "file.dat")
	}
	if info.Size != 42000 {
		t.Errorf("size = %d, want 42000", info.Size)
	}
}

func TestMarshalFileInfoLowercase(t *testing.T) {
	offer := &FileOffer{
		Name: "MyFile.TXT",
		Size: 100,
	}
	data := marshalFileInfo(offer, 0, 0)

	info, err := parseFileInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "myfile.txt" {
		t.Errorf("name = %q, want lowercase %q", info.Name, "myfile.txt")
	}
}

func TestMarshalFileInfoBackslash(t *testing.T) {
	offer := &FileOffer{
		Name: "path\\to\\file.dat",
		Size: 100,
	}
	data := marshalFileInfo(offer, 0, 0)

	info, err := parseFileInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "path/to/file.dat" {
		t.Errorf("name = %q, want %q", info.Name, "path/to/file.dat")
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"test.txt", "test.txt"},
		{"../../../etc/passwd", "passwd"},
		{"/absolute/path/file.dat", "file.dat"},
		{"path/to/file.bin", "file.bin"},
		{"", "."},
	}

	for _, tc := range tests {
		got := SanitizeFilename(tc.input)
		if got != tc.expected {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}
