package cursor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"
)

func TestFrameMemoryBounds(t *testing.T) {
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], maxFrameBytes+1)
	if _, err := readFrame(bufio.NewReader(bytes.NewReader(header))); err == nil {
		t.Fatal("oversized frame accepted")
	}
	var data bytes.Buffer
	zipper := gzip.NewWriter(&data)
	zipper.Write(bytes.Repeat([]byte("x"), maxFrameBytes+1))
	zipper.Close()
	compressed := testFrame(flagCompressed, data.Bytes())
	if _, err := readFrame(bufio.NewReader(bytes.NewReader(compressed))); err == nil {
		t.Fatal("oversized decompressed frame accepted")
	}
	if _, err := readFrame(bufio.NewReader(bytes.NewReader(testFrame(128, nil)))); err == nil {
		t.Fatal("unknown frame flags accepted")
	}
}
