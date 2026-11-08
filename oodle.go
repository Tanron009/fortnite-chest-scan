package main

// Pure Go decoder for Oodle "Mermaid" compressed data, the compression Fortnite uses for replay data chunks.
//
// This is a clean-room style Go port of the Mermaid decode path of powzix/ooz (GPLv3, based on the format
// documentation by Rarefied Radish/Powzix). Because it derives from ooz, this file and the program built with
// it are licensed GPLv3 as well. Only the parts needed for Mermaid are here (stored streams, both LZ modes);
// entropy coded sub-streams (Huffman/tANS/RLE) are not implemented and are reported as an error.

import (
	"errors"
	"fmt"
)

var refDecompressFn func(src []byte, dstLen int) ([]byte, error) // optional reference decoder (dev builds only)

func init() { decompressFn = goDecompress }

const (
	decoderMermaid = 10
	quantum        = 0x40000
)

func goDecompress(src []byte, dstLen int) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("oodle: corrupt data (%v)", r)
		}
	}()
	dst := make([]byte, dstLen)
	sp, dp := 0, 0
	var restart, uncompressed, checksums bool
	var dtype int
	for dp < dstLen {
		if dp&(quantum-1) == 0 {
			if len(src)-sp < 2 {
				return nil, errors.New("oodle: truncated header")
			}
			b0, b1 := src[sp], src[sp+1]
			if b0&0xF != 0xC || (b0>>4)&3 != 0 {
				return nil, errors.New("oodle: bad block header")
			}
			restart = b0>>7&1 == 1
			_ = restart
			uncompressed = b0>>6&1 == 1
			dtype = int(b1 & 0x7F)
			checksums = b1>>7&1 == 1
			sp += 2
		}
		if dtype != decoderMermaid {
			return nil, fmt.Errorf("oodle: decoder type %d not supported (only Mermaid)", dtype)
		}
		n := dstLen - dp
		if n > quantum {
			n = quantum
		}
		if uncompressed {
			if len(src)-sp < n {
				return nil, errors.New("oodle: truncated stored block")
			}
			copy(dst[dp:dp+n], src[sp:sp+n])
			sp += n
			dp += n
			continue
		}
		// quantum header
		if len(src)-sp < 3 {
			return nil, errors.New("oodle: truncated quantum header")
		}
		v := uint32(src[sp])<<16 | uint32(src[sp+1])<<8 | uint32(src[sp+2])
		size := v & 0x3FFFF
		var comp int
		var crc uint32
		if size != 0x3FFFF {
			comp = int(size) + 1
			if checksums {
				if len(src)-sp < 6 {
					return nil, errors.New("oodle: truncated quantum header")
				}
				crc = uint32(src[sp+3])<<16 | uint32(src[sp+4])<<8 | uint32(src[sp+5])
				sp += 6
			} else {
				sp += 3
			}
		} else {
			if v>>18 != 1 || len(src)-sp < 4 {
				return nil, errors.New("oodle: bad quantum header")
			}
			crc = uint32(src[sp+3])
			sp += 4
			comp = 0
		}
		_ = crc
		if comp == 0 { // block filled with a single byte
			for i := 0; i < n; i++ {
				dst[dp+i] = byte(crc)
			}
			dp += n
			continue
		}
		if len(src)-sp < comp {
			return nil, errors.New("oodle: truncated quantum")
		}
		if comp > dstLen-dp {
			return nil, errors.New("oodle: invalid quantum size")
		}
		if comp == n { // stored
			copy(dst[dp:dp+n], src[sp:sp+n])
		} else {
			used, e := mermaidQuantum(dst, dp, dp+n, src[sp:sp+comp])
			if e != nil {
				return nil, e
			}
			if used != comp {
				return nil, fmt.Errorf("oodle: quantum used %d of %d bytes", used, comp)
			}
		}
		sp += comp
		dp += n
	}
	return dst, nil
}

// decodeStoredBytes reads a stream descriptor; only "stored" (type 0) streams are supported.
func decodeStoredBytes(src []byte, maxOut int) (stream []byte, used int, err error) {
	if len(src) < 2 {
		return nil, 0, errors.New("oodle: stream truncated")
	}
	if (src[0]>>4)&7 != 0 {
		return nil, 0, fmt.Errorf("oodle: entropy coded stream type %d not supported", (src[0]>>4)&7)
	}
	var n, hdr int
	if src[0] >= 0x80 {
		n = (int(src[0])<<8 | int(src[1])) & 0xFFF
		hdr = 2
	} else {
		if len(src) < 3 {
			return nil, 0, errors.New("oodle: stream truncated")
		}
		n = int(src[0])<<16 | int(src[1])<<8 | int(src[2])
		if n&^0x3ffff != 0 {
			return nil, 0, errors.New("oodle: reserved bits set")
		}
		hdr = 3
	}
	if n > maxOut || len(src)-hdr < n {
		return nil, 0, errors.New("oodle: stream size out of range")
	}
	return src[hdr : hdr+n], hdr + n, nil
}

type lzTable struct {
	cmd                        []byte
	cmd2Offsets, cmd2OffsetsEn int
	length                     []byte
	lengthPos                  int
	lit                        []byte
	litPos                     int
	off16                      []uint16
	off16Pos                   int
	off32a, off32b             []uint32
}

func mermaidQuantum(dst []byte, dp, dend int, src []byte) (int, error) {
	sp := 0
	for dp < dend {
		cnt := dend - dp
		if cnt > 0x20000 {
			cnt = 0x20000
		}
		if len(src)-sp < 4 {
			return 0, errors.New("oodle: less than 4 bytes left in quantum")
		}
		ch := int(src[sp+2]) | int(src[sp+1])<<8 | int(src[sp])<<16
		var used int
		if ch&0x800000 == 0 { // stored without match copying
			s, u, err := decodeStoredBytes(src[sp:], cnt)
			if err != nil || len(s) != cnt {
				return 0, errors.New("oodle: bad stored chunk")
			}
			copy(dst[dp:dp+cnt], s)
			used = u
		} else {
			sp += 3
			used = ch & 0x7FFFF
			mode := (ch >> 19) & 0xF
			if len(src)-sp < used {
				return 0, errors.New("oodle: not enough source bytes")
			}
			chunk := src[sp : sp+used]
			if used < cnt {
				var lz lzTable
				if err := mermaidReadLzTable(mode, chunk, dst, dp, cnt, &lz); err != nil {
					return 0, err
				}
				if err := mermaidProcessRuns(mode, chunk, dst, dp, cnt, &lz); err != nil {
					return 0, err
				}
			} else if used > cnt || mode != 0 {
				return 0, errors.New("oodle: bad chunk sizes")
			} else {
				copy(dst[dp:dp+cnt], chunk)
			}
		}
		sp += used
		dp += cnt
	}
	return sp, nil
}

func le16(b []byte) int { return int(b[0]) | int(b[1])<<8 }
func le24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }

func mermaidReadLzTable(mode int, src []byte, dst []byte, dp, cnt int, lz *lzTable) error {
	if mode > 1 || len(src) < 10 {
		return errors.New("oodle: bad LZ table")
	}
	offset := dp
	sp := 0
	if offset == 0 {
		copy(dst[0:8], src[0:8])
		sp += 8
	}
	maxScratch := 0x6C000
	lit, n, err := decodeStoredBytes(src[sp:], min(maxScratch, cnt))
	if err != nil {
		return err
	}
	sp += n
	lz.lit = lit
	cmd, n, err := decodeStoredBytes(src[sp:], min(maxScratch, cnt))
	if err != nil {
		return err
	}
	sp += n
	lz.cmd = cmd
	lz.cmd2OffsetsEn = len(cmd)
	if cnt <= 0x10000 {
		lz.cmd2Offsets = len(cmd)
	} else {
		if len(src)-sp < 2 {
			return errors.New("oodle: truncated")
		}
		lz.cmd2Offsets = le16(src[sp:])
		sp += 2
		if lz.cmd2Offsets > lz.cmd2OffsetsEn {
			return errors.New("oodle: bad cmd split")
		}
	}
	if len(src)-sp < 2 {
		return errors.New("oodle: truncated")
	}
	off16Count := le16(src[sp:])
	if off16Count == 0xFFFF {
		sp += 2
		hi, n, err := decodeStoredBytes(src[sp:], cnt>>1)
		if err != nil {
			return err
		}
		sp += n
		lo, n, err := decodeStoredBytes(src[sp:], cnt>>1)
		if err != nil {
			return err
		}
		sp += n
		if len(lo) != len(hi) {
			return errors.New("oodle: offset16 size mismatch")
		}
		lz.off16 = make([]uint16, len(lo))
		for i := range lo {
			lz.off16[i] = uint16(int(lo[i]) + int(hi[i])*256)
		}
	} else {
		if len(src)-sp < 2+off16Count*2 {
			return errors.New("oodle: truncated offset16 stream")
		}
		lz.off16 = make([]uint16, off16Count)
		for i := 0; i < off16Count; i++ {
			lz.off16[i] = uint16(le16(src[sp+2+i*2:]))
		}
		sp += 2 + off16Count*2
	}
	if len(src)-sp < 3 {
		return errors.New("oodle: truncated")
	}
	t := le24(src[sp:])
	sp += 3
	if t != 0 {
		s1, s2 := t>>12, t&0xFFF
		if s1 == 4095 {
			if len(src)-sp < 2 {
				return errors.New("oodle: truncated")
			}
			s1 = le16(src[sp:])
			sp += 2
		}
		if s2 == 4095 {
			if len(src)-sp < 2 {
				return errors.New("oodle: truncated")
			}
			s2 = le16(src[sp:])
			sp += 2
		}
		var u int
		lz.off32a, u, err = farOffsets(src[sp:], s1, offset)
		if err != nil {
			return err
		}
		sp += u
		lz.off32b, u, err = farOffsets(src[sp:], s2, offset+0x10000)
		if err != nil {
			return err
		}
		sp += u
	}
	lz.length = src[sp:]
	return nil
}

func farOffsets(src []byte, count int, offset int) ([]uint32, int, error) {
	out := make([]uint32, count)
	p := 0
	for i := 0; i < count; i++ {
		if len(src)-p < 3 {
			return nil, 0, errors.New("oodle: truncated far offsets")
		}
		off := le24(src[p:])
		p += 3
		if offset >= 0xC00000-1 && off >= 0xC00000 {
			if p >= len(src) {
				return nil, 0, errors.New("oodle: truncated far offsets")
			}
			off += int(src[p]) << 22
			p++
		}
		if off > offset {
			return nil, 0, errors.New("oodle: far offset out of range")
		}
		out[i] = uint32(off)
	}
	return out, p, nil
}

func mermaidProcessRuns(mode int, src []byte, dst []byte, dp, size int, lz *lzTable) error {
	offset := dp
	saved := -8
	cmdAll := lz.cmd
	for iter := 0; iter < 2; iter++ {
		cur := size
		if cur > 0x10000 {
			cur = 0x10000
		}
		var cmd []byte
		var off32 []uint32
		if iter == 0 {
			off32 = lz.off32a
			cmd = cmdAll[:lz.cmd2Offsets]
		} else {
			off32 = lz.off32b
			cmd = cmdAll[lz.cmd2Offsets:lz.cmd2OffsetsEn]
		}
		start := 0
		if offset == 0 && iter == 0 {
			start = 8
		}
		if err := mermaidRuns(mode == 0, dst, dp, cur, cmd, off32, lz, &saved, start); err != nil {
			return err
		}
		dp += cur
		size -= cur
		if size == 0 {
			break
		}
	}
	if lz.lengthPos != len(lz.length) {
		return errors.New("oodle: length stream not fully consumed")
	}
	return nil
}

func mermaidRuns(delta bool, dst []byte, begin, size int, cmd []byte, off32 []uint32, lz *lzTable, saved *int, startOff int) error {
	end := begin + size
	d := begin + startOff
	recent := *saved
	o32 := 0
	errc := errors.New("oodle: corrupt Mermaid stream")

	readLen := func() (int, bool) {
		if lz.lengthPos >= len(lz.length) {
			return 0, false
		}
		l := int(lz.length[lz.lengthPos])
		if l > 251 {
			if len(lz.length)-lz.lengthPos < 3 {
				return 0, false
			}
			l += le16(lz.length[lz.lengthPos+1:]) * 4
			lz.lengthPos += 2
		}
		lz.lengthPos++
		return l, true
	}
	copyMatch := func(from, n int) error {
		if from < 0 || d+n > len(dst) {
			return errc
		}
		for i := 0; i < n; i++ {
			dst[d+i] = dst[from+i]
		}
		d += n
		return nil
	}
	copyLits := func(n int) error {
		if lz.litPos+n > len(lz.lit) || d+n > len(dst) {
			return errc
		}
		if delta {
			for i := 0; i < n; i++ {
				if d+i+recent < 0 {
					return errc
				}
				dst[d+i] = lz.lit[lz.litPos+i] + dst[d+i+recent]
			}
		} else {
			copy(dst[d:d+n], lz.lit[lz.litPos:lz.litPos+n])
		}
		d += n
		lz.litPos += n
		return nil
	}

	for _, flag := range cmd {
		f := int(flag)
		switch {
		case f >= 24:
			litLen := f & 7
			useNew := f>>7 == 0
			if err := copyLits(litLen); err != nil {
				return err
			}
			if useNew {
				if lz.off16Pos >= len(lz.off16) {
					return errc
				}
				recent = -int(lz.off16[lz.off16Pos])
				lz.off16Pos++
			}
			if err := copyMatch(d+recent, (f>>3)&0xF); err != nil {
				return err
			}
		case f > 2:
			n := f + 5
			if o32 >= len(off32) {
				return errc
			}
			from := begin - int(off32[o32])
			o32++
			recent = from - d
			if end-d < n {
				return errc
			}
			if err := copyMatch(from, n); err != nil {
				return err
			}
		case f == 0:
			l, ok := readLen()
			if !ok {
				return errc
			}
			l += 64
			if end-d < l || len(lz.lit)-lz.litPos < l {
				return errc
			}
			if err := copyLits(l); err != nil {
				return err
			}
		case f == 1:
			l, ok := readLen()
			if !ok || lz.off16Pos >= len(lz.off16) {
				return errc
			}
			l += 91
			from := d - int(lz.off16[lz.off16Pos])
			lz.off16Pos++
			recent = from - d
			if err := copyMatch(from, l); err != nil {
				return err
			}
		default: // 2
			l, ok := readLen()
			if !ok || o32 >= len(off32) {
				return errc
			}
			l += 29
			from := begin - int(off32[o32])
			o32++
			recent = from - d
			if err := copyMatch(from, l); err != nil {
				return err
			}
		}
	}
	if rest := end - d; rest > 0 {
		if err := copyLits(rest); err != nil {
			return err
		}
	}
	*saved = recent
	return nil
}
