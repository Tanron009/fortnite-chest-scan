package main

import (
	"encoding/binary"
	"math"
	"unicode/utf16"
)

// bitReader reads an Unreal bit stream: bit i of the stream is (buf[i>>3] >> (i&7)) & 1.
type bitReader struct {
	buf   []byte
	pos   int // bit position
	end   int // bit end (exclusive)
	err   bool
	stack []int // saved ends for nested limits
}

func newBitReader(buf []byte, bits int) *bitReader {
	return &bitReader{buf: buf, end: bits}
}

func (r *bitReader) atEnd() bool   { return r.pos >= r.end }
func (r *bitReader) bitsLeft() int { return r.end - r.pos }

// limit restricts reading to the next n bits; unlimit restores the previous end.
func (r *bitReader) limit(n int) {
	r.stack = append(r.stack, r.end)
	if r.pos+n > r.end {
		r.err = true
		n = r.end - r.pos
	}
	r.end = r.pos + n
}
func (r *bitReader) unlimit() {
	r.end = r.stack[len(r.stack)-1]
	r.stack = r.stack[:len(r.stack)-1]
}

func (r *bitReader) bit() bool {
	if r.pos >= r.end {
		r.err = true
		return false
	}
	v := (r.buf[r.pos>>3] >> (uint(r.pos) & 7)) & 1
	r.pos++
	return v == 1
}

func (r *bitReader) bits(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if r.bit() {
			v |= 1 << uint(i)
		}
	}
	return v
}

func (r *bitReader) skip(n int) {
	if r.pos+n > r.end {
		r.err = true
		r.pos = r.end
		return
	}
	r.pos += n
}

func (r *bitReader) u8() byte     { return byte(r.bits(8)) }
func (r *bitReader) u16() uint16  { return uint16(r.bits(16)) }
func (r *bitReader) u32() uint32  { return uint32(r.bits(32)) }
func (r *bitReader) i32() int32   { return int32(r.bits(32)) }
func (r *bitReader) f32() float32 { return math.Float32frombits(r.u32()) }
func (r *bitReader) f64() float64 { return math.Float64frombits(r.bits(64)) }

// serializedInt is FBitReader::SerializeInt(value, max).
func (r *bitReader) serializedInt(max int) uint32 {
	var v uint32
	for mask := uint32(1); v+mask < uint32(max) && mask != 0; mask *= 2 {
		if r.bit() {
			v |= mask
		}
	}
	return v
}

// intPacked is FArchive::SerializeIntPacked: 7 bits per byte, low bit = "more".
func (r *bitReader) intPacked() uint32 {
	var v uint32
	for i := 0; i < 5; i++ {
		b := r.u8()
		if r.err {
			return v
		}
		v |= uint32(b>>1) << (7 * uint(i))
		if b&1 == 0 {
			break
		}
	}
	return v
}

func (r *bitReader) bytes(n int) []byte {
	if n < 0 || r.pos+n*8 > r.end {
		r.err = true
		return nil
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = r.u8()
	}
	return out
}

func (r *bitReader) fstring() string {
	n := r.i32()
	if r.err || n == 0 {
		return ""
	}
	if n > 0 {
		if n > 1<<16 {
			r.err = true
			return ""
		}
		b := r.bytes(int(n))
		if len(b) > 0 && b[len(b)-1] == 0 {
			b = b[:len(b)-1]
		}
		return string(b)
	}
	cnt := int(-n)
	if cnt > 1<<16 {
		r.err = true
		return ""
	}
	b := r.bytes(cnt * 2)
	if b == nil {
		return ""
	}
	u := make([]uint16, 0, cnt)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// fname: hard-coded engine names are sent as an index; others as string + number.
func (r *bitReader) fname() string {
	if r.bit() {
		idx := r.intPacked()
		return hardcodedName(idx)
	}
	s := r.fstring()
	r.i32()
	return s
}

// archive is what the object/export readers need; implemented by bitReader and byteArchive.
type archive interface {
	intPacked() uint32
	u8() byte
	u32() uint32
	fstring() string
	failed() bool
}

func (r *bitReader) failed() bool { return r.err }

// byteArchive wraps byteReader (container level, whole bytes) with the same helpers.
type byteArchive struct{ *byteReader }

func (b byteArchive) intPacked() uint32 {
	var v uint32
	for i := 0; i < 5; i++ {
		x := b.byteReader.u8()
		if b.err != nil {
			return v
		}
		v |= uint32(x>>1) << (7 * uint(i))
		if x&1 == 0 {
			break
		}
	}
	return v
}
func (b byteArchive) u8() byte        { return b.byteReader.u8() }
func (b byteArchive) u32() uint32     { return b.byteReader.u32() }
func (b byteArchive) fstring() string { return b.byteReader.fstring() }
func (b byteArchive) failed() bool    { return b.err != nil }
