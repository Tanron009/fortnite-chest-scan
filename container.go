package main

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

// Unreal "local file" replay container: file info, then a sequence of chunks.
const (
	fileMagic    = 0x1CA2E27F
	networkMagic = 0x2CF5A13D

	chunkHeader     = 0
	chunkReplayData = 1
	chunkCheckpoint = 2
	chunkEvent      = 3
)

type byteReader struct {
	b   []byte
	pos int
	err error
}

func (r *byteReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if n < 0 || r.pos+n > len(r.b) {
		r.err = fmt.Errorf("read of %d bytes at %d past end (%d)", n, r.pos, len(r.b))
		return false
	}
	return true
}
func (r *byteReader) u8() byte {
	if !r.need(1) {
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}
func (r *byteReader) u16() uint16 {
	if !r.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}
func (r *byteReader) u32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}
func (r *byteReader) i32() int32 { return int32(r.u32()) }
func (r *byteReader) u64() uint64 {
	if !r.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v
}
func (r *byteReader) f32() float32 { return math.Float32frombits(r.u32()) }
func (r *byteReader) bytes(n int) []byte {
	if !r.need(n) {
		return nil
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v
}
func (r *byteReader) skip(n int)  { r.bytes(n) }
func (r *byteReader) atEnd() bool { return r.pos >= len(r.b) }

// fstring: Unreal FString: int32 length; >0 ANSI (incl. NUL), <0 UTF-16 (-len chars incl. NUL).
func (r *byteReader) fstring() string {
	n := r.i32()
	if r.err != nil || n == 0 {
		return ""
	}
	if n > 0 {
		if n > 1<<20 {
			r.err = errors.New("fstring too long")
			return ""
		}
		raw := r.bytes(int(n))
		if raw == nil {
			return ""
		}
		return string(raw[:len(raw)-1])
	}
	cnt := int(-n)
	if cnt > 1<<20 {
		r.err = errors.New("fstring too long")
		return ""
	}
	raw := r.bytes(cnt * 2)
	if raw == nil {
		return ""
	}
	u := make([]uint16, cnt-1)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(u))
}

type ReplayInfo struct {
	FileVersion  uint32
	LengthMs     uint32
	NetVersion   uint32
	Changelist   uint32
	FriendlyName string
	IsLive       bool
	IsCompressed bool
	IsEncrypted  bool
	Key          []byte
}

type Chunk struct {
	Type   uint32
	Offset int // offset of chunk payload in file
	Size   int
	Data   []byte
}

func parseFileInfo(r *byteReader) (*ReplayInfo, error) {
	if r.u32() != fileMagic {
		return nil, errors.New("not a replay file (bad magic)")
	}
	info := &ReplayInfo{}
	info.FileVersion = r.u32()
	if info.FileVersion >= 7 {
		r.skip(int(r.i32()) * 20) // custom version table
	}
	info.LengthMs = r.u32()
	info.NetVersion = r.u32()
	info.Changelist = r.u32()
	info.FriendlyName = r.fstring()
	info.IsLive = r.u32() != 0
	if info.FileVersion >= 3 {
		r.u64() // timestamp
	}
	if info.FileVersion >= 2 {
		info.IsCompressed = r.u32() != 0
	}
	if info.FileVersion >= 6 {
		info.IsEncrypted = r.u32() != 0
		n := int(r.u32())
		info.Key = append([]byte(nil), r.bytes(n)...)
	}
	if r.err != nil {
		return nil, r.err
	}
	if info.IsEncrypted && len(info.Key) == 0 {
		return nil, errors.New("replay is encrypted but has no key (recorded match not finished?)")
	}
	return info, nil
}

func readChunks(r *byteReader) ([]Chunk, error) {
	var chunks []Chunk
	for r.pos+8 <= len(r.b) {
		t := r.u32()
		size := int(r.i32())
		if size <= 0 || r.pos+size > len(r.b) {
			return chunks, fmt.Errorf("bad chunk (type %d size %d) at %d", t, size, r.pos)
		}
		chunks = append(chunks, Chunk{Type: t, Offset: r.pos, Size: size, Data: r.b[r.pos : r.pos+size]})
		r.pos += size
	}
	return chunks, nil
}

// decryptAESECB decrypts AES (key size from key) in ECB mode and strips PKCS7 padding.
func decryptAESECB(key, data []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not a multiple of 16", len(data))
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		blk.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	if len(out) == 0 {
		return nil, errors.New("empty chunk")
	}
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("bad padding (wrong key?)")
	}
	return out[:len(out)-pad], nil
}

// decompressFn is the Oodle (Mermaid) decompressor; set in an init() of another file.
var decompressFn func(src []byte, dstLen int) ([]byte, error)

// dataChunk returns the (decrypted, decompressed) payload of a ReplayData chunk.
func dataChunk(info *ReplayInfo, c Chunk) ([]byte, error) {
	r := &byteReader{b: c.Data}
	length := len(c.Data)
	if info.FileVersion >= 4 {
		r.u32() // start time
		r.u32() // end time
		length = int(r.u32())
	}
	if info.FileVersion >= 6 {
		r.i32() // memory size in bytes
	}
	body := r.bytes(length)
	if r.err != nil {
		return nil, r.err
	}
	var err error
	if info.IsEncrypted {
		body, err = decryptAESECB(info.Key, body)
		if err != nil {
			return nil, err
		}
	}
	if info.IsCompressed {
		rr := &byteReader{b: body}
		decSize := int(rr.i32())
		compSize := int(rr.i32())
		comp := rr.bytes(compSize)
		if rr.err != nil {
			return nil, rr.err
		}
		return decompressFn(comp, decSize)
	}
	return body, nil
}
