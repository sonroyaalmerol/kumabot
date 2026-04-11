package player

import (
	"bufio"
	"encoding/binary"
	"io"
	"testing"
)

// makePCMFrame creates a valid PCM frame (16-byte header + 3840 bytes data) in a byte slice.
func makePCMFrame(pts48 int64) []byte {
	const dataLen = 960 * 2 * 2
	buf := make([]byte, 16+dataLen)
	binary.BigEndian.PutUint64(buf[0:8], uint64(pts48))
	binary.BigEndian.PutUint32(buf[8:12], 960)
	binary.BigEndian.PutUint32(buf[12:16], uint32(dataLen))
	// data section is all zeros (silence) — fine for benchmarking
	return buf
}

// BenchmarkReadPCMFrame measures the hot-path frame reader with buffer reuse.
func BenchmarkReadPCMFrame(b *testing.B) {
	frame := makePCMFrame(0)
	r := bufio.NewReaderSize(newBytesReader(frame), 128*1024)
	readBuf := make([]byte, 0, 960*2*2)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		f, err := readPCMFrame(r, readBuf)
		if err != nil {
			// reader exhausted — refill
			r.Reset(newBytesReader(frame))
			continue
		}
		readBuf = f.data[:0]
	}
}

// BenchmarkReadPCMFrameNoReuse measures without buffer reuse to show the difference.
func BenchmarkReadPCMFrameNoReuse(b *testing.B) {
	frame := makePCMFrame(0)
	r := bufio.NewReaderSize(newBytesReader(frame), 128*1024)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := readPCMFrame(r, nil)
		if err != nil {
			r.Reset(newBytesReader(frame))
			continue
		}
	}
}

// bytesReader is a minimal io.Reader that resets to the same bytes.
// Avoids importing bytes just for the benchmark.
type bytesReader struct {
	data []byte
	pos  int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *bytesReader) Reset(data []byte) {
	r.data = data
	r.pos = 0
}
