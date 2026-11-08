package main

import (
	"fmt"
	"math/bits"
	"strings"
)

// ---- engine / replay version thresholds we depend on -------------------------------------------------
const (
	netHistoryMultipleLevels    = 6
	netHistoryHeaderFlags       = 9
	netHistoryLevelStreamingFix = 10
	netHistorySaveFullEngine    = 11
	netHistoryHeaderGUID        = 12
	netHistorySavePackageVer    = 17
	netHistoryRecordingMeta     = 18
	netHistoryUseCustomVersion  = 19

	engNewActorOverrideLevel  = 5
	engChannelNames           = 6
	engChannelCloseReason     = 7
	engAcksInHeader           = 8
	engNetExportSerialization = 9
	engNetExportSerializeFix  = 10
	engOptionallyQuantize     = 13
	engPackedVectorLWC        = 23
	engSubobjectOuterChain    = 18
	engSubobjectDestroyFlag   = 30
	engCustomExports          = 36

	flagHasStreamingFixes     = 1 << 1
	flagGameSpecificFrameData = 1 << 3

	maxPacketBits = 16384
)

type Export struct {
	Handle uint32
	Name   string
}

type Group struct {
	Path    string
	Fixed   string
	Index   uint32
	Exports []*Export
	hasName map[string]bool
}

func (g *Group) set(e *Export) {
	if int(e.Handle) < len(g.Exports) {
		g.Exports[e.Handle] = e
		g.hasName[e.Name] = true
	}
}

type Channel struct {
	Index     uint32
	Actor     uint32 // actor net guid
	Archetype uint32 // archetype net guid (0 for stably named actors)
	opened    bool
}

type partialBunch struct {
	data      []byte
	nbits     int
	chSeq     uint32
	reliable  bool
	open      bool
	hasExport bool
}

type Parser struct {
	netVersion uint32
	engVersion uint32
	flags      uint32

	groups       map[string]*Group
	groupByIndex map[uint32]*Group
	guidPath     map[uint32]string
	chans        map[uint32]*Channel
	partials     map[uint32]*partialBunch
	groupCache   map[uint32]*Group
	groupCacheN  int

	time        float32
	inPacketID  uint32
	inReliable  uint32
	stats       struct{ frames, packets, bunches, packetErrs, panics int }
	Names       map[uint32]string
	OnProps     func(p *Parser, ch *Channel, g *Group, vals map[string]uint32)
	OnChanOpen  func(p *Parser, ch *Channel)
	OnChanClose func(p *Parser, ch *Channel)
}

func NewParser() *Parser {
	return &Parser{
		groups:       map[string]*Group{},
		groupByIndex: map[uint32]*Group{},
		guidPath:     map[uint32]string{},
		chans:        map[uint32]*Channel{},
		partials:     map[uint32]*partialBunch{},
		groupCache:   map[uint32]*Group{},
		Names:        map[uint32]string{},
	}
}

// removeAllPathPrefixes keeps the object name of "/Pkg/Path.Name"; "Default__X" -> "X".
func removeAllPathPrefixes(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		switch p[i] {
		case '.':
			return p[i+1:]
		case '/':
			return p
		}
	}
	return strings.TrimPrefix(p, "Default__")
}

// ---- header chunk ---------------------------------------------------------------------------------------
func (p *Parser) ReadHeader(data []byte) error {
	r := &byteReader{b: data}
	if r.u32() != networkMagic {
		return fmt.Errorf("bad demo header magic")
	}
	p.netVersion = r.u32()
	if p.netVersion >= netHistoryUseCustomVersion {
		r.skip(int(r.i32()) * 20)
	}
	r.u32() // network checksum
	p.engVersion = r.u32()
	r.u32() // game network protocol
	if p.netVersion >= netHistoryHeaderGUID {
		r.skip(16)
	}
	if p.netVersion >= netHistorySaveFullEngine {
		r.u16()
		r.u16()
		r.u16()
		r.u32()
		r.fstring()
	} else {
		r.u32()
	}
	if p.netVersion >= netHistoryRecordingMeta {
		r.u32()
		r.u32()
		r.u32()
	}
	n := int(r.i32()) // level names and times
	for i := 0; i < n && r.err == nil; i++ {
		r.fstring()
		r.u32()
	}
	if p.netVersion >= netHistoryHeaderFlags {
		p.flags = r.u32()
	}
	if r.err != nil {
		return r.err
	}
	if p.engVersion < engNetExportSerializeFix || p.netVersion < netHistoryLevelStreamingFix || p.engVersion < engOptionallyQuantize {
		return fmt.Errorf("replay from an old game build (net %d, engine %d) is not supported", p.netVersion, p.engVersion)
	}
	return nil
}

// ---- frames -----------------------------------------------------------------------------------------------
// ReadChunkData parses all demo frames in one decompressed ReplayData chunk.
func (p *Parser) ReadChunkData(data []byte) {
	r := &byteReader{b: data}
	for !r.atEnd() && r.err == nil {
		if !p.readFrame(r) {
			return
		}
		p.stats.frames++
	}
}

func (p *Parser) readFrame(r *byteReader) bool {
	if p.netVersion >= netHistoryMultipleLevels {
		r.i32() // current level index
	}
	p.time = r.f32()
	a := byteArchive{r}
	// export data: net field exports, then net guids
	p.readNetFieldExports(r)
	numGuids := int(a.intPacked())
	for i := 0; i < numGuids && r.err == nil; i++ {
		size := int(r.i32())
		blob := r.bytes(size)
		if r.err != nil {
			return false
		}
		sub := byteArchive{&byteReader{b: blob}}
		p.internalLoadObject(sub, true, 0)
	}
	if p.flags&flagHasStreamingFixes != 0 {
		n := int(a.intPacked())
		for i := 0; i < n && r.err == nil; i++ {
			r.fstring()
		}
		r.u64() // external offset
	} else {
		return false // legacy streaming layout not supported
	}
	// external data
	for r.err == nil {
		nbits := int(a.intPacked())
		if nbits == 0 {
			break
		}
		a.intPacked() // net guid
		r.skip((nbits + 7) >> 3)
	}
	if p.flags&flagGameSpecificFrameData != 0 {
		skip := r.u64()
		if skip > 0 {
			r.skip(int(skip))
		}
	}
	for r.err == nil {
		a.intPacked() // seen level index
		size := int(r.i32())
		if size == 0 {
			break
		}
		if size < 0 || size > 2048 {
			return false
		}
		pkt := r.bytes(size)
		if r.err != nil {
			return false
		}
		p.receivedRawPacket(pkt)
	}
	return r.err == nil
}

func (p *Parser) readNetFieldExports(r *byteReader) {
	a := byteArchive{r}
	n := int(a.intPacked())
	for i := 0; i < n && r.err == nil; i++ {
		pathIndex := a.intPacked()
		exported := a.intPacked() == 1
		var g *Group
		if exported {
			path := r.fstring()
			num := a.intPacked()
			g = p.getOrCreateGroup(path, pathIndex, num)
		} else {
			g = p.groupByIndex[pathIndex]
		}
		e := p.readNetFieldExportByte(r)
		if g != nil && e != nil {
			g.set(e)
		}
	}
}

func (p *Parser) getOrCreateGroup(path string, index uint32, num uint32) *Group {
	if num > 1<<20 {
		num = 1 << 20
	}
	g, ok := p.groups[path]
	if !ok {
		g = &Group{Path: path, Fixed: removeAllPathPrefixes(path), Index: index, Exports: make([]*Export, num), hasName: map[string]bool{}}
		p.groups[path] = g
		p.groupByIndex[index] = g
	} else if int(num) > len(g.Exports) {
		ne := make([]*Export, num)
		copy(ne, g.Exports)
		g.Exports = ne
	}
	return g
}

func (p *Parser) readNetFieldExportByte(r *byteReader) *Export {
	if r.u8() == 0 { // not exported
		return nil
	}
	a := byteArchive{r}
	e := &Export{Handle: a.intPacked()}
	r.u32() // compatible checksum
	// FName (byte archive): hard-coded flag byte, then index or (string, number)
	if r.u8() != 0 {
		e.Name = hardcodedName(a.intPacked())
	} else {
		e.Name = r.fstring()
		r.i32()
	}
	return e
}

func (p *Parser) readNetFieldExportBit(r *bitReader) *Export {
	if !r.bit() {
		return nil
	}
	e := &Export{Handle: r.intPacked()}
	r.u32()
	e.Name = r.fname()
	return e
}

// internalLoadObject reads an object reference; when exporting it records the object's path.
func (p *Parser) internalLoadObject(a archive, exporting bool, depth int) uint32 {
	if depth > 16 {
		return 0
	}
	guid := a.intPacked()
	if guid == 0 || a.failed() {
		return guid
	}
	if guid == 1 || exporting {
		flags := a.u8()
		if flags&1 != 0 { // has path
			p.internalLoadObject(a, true, depth+1)
			path := a.fstring()
			if flags&4 != 0 {
				a.u32()
			}
			if exporting {
				p.guidPath[guid] = removeAllPathPrefixes(path)
			}
		}
	}
	return guid
}

// ---- packets / bunches ---------------------------------------------------------------------------------
func (p *Parser) receivedRawPacket(pkt []byte) {
	last := pkt[len(pkt)-1]
	if last == 0 {
		p.stats.packetErrs++
		return
	}
	nbits := len(pkt)*8 - 1 - bits.LeadingZeros8(last)
	r := newBitReader(pkt, nbits)
	defer func() {
		if e := recover(); e != nil {
			p.stats.panics++
		}
	}()
	p.stats.packets++
	p.receivedPacket(r)
}

type bunchHdr struct {
	open, close, dormant, replPaused, reliable        bool
	hasExports, hasMustMap, partial, pInitial, pFinal bool
	chIndex, chSeq                                    uint32
}

func (p *Parser) receivedPacket(r *bitReader) {
	p.inPacketID++
	for !r.atEnd() {
		var b bunchHdr
		control := r.bit()
		b.open = control && r.bit()
		b.close = control && r.bit()
		if b.close {
			reason := r.serializedInt(15)
			b.dormant = reason == 1
		}
		b.replPaused = r.bit()
		b.reliable = r.bit()
		b.chIndex = r.intPacked()
		b.hasExports = r.bit()
		b.hasMustMap = r.bit()
		b.partial = r.bit()
		if b.reliable {
			b.chSeq = p.inReliable + 1
		} else if b.partial {
			b.chSeq = p.inPacketID
		}
		b.pInitial = b.partial && r.bit()
		hasCustomFinal := false
		if b.partial && p.engVersion >= engCustomExports {
			hasCustomFinal = r.bit()
		}
		_ = hasCustomFinal
		b.pFinal = b.partial && r.bit()
		if b.reliable || b.open {
			r.fname() // channel name
		}
		dataBits := int(r.serializedInt(maxPacketBits))
		if r.err {
			return
		}
		p.stats.bunches++

		var br *bitReader
		if b.partial {
			if r.bitsLeft() < dataBits {
				return
			}
			raw := make([]byte, (dataBits+7)/8)
			for i := 0; i < dataBits; i++ {
				if r.bit() {
					raw[i>>3] |= 1 << uint(i&7)
				}
			}
			br = newBitReader(raw, dataBits)
		} else {
			if r.bitsLeft() < dataBits {
				return
			}
			br = newBitReader(r.buf, r.pos+dataBits)
			br.pos = r.pos
			r.pos += dataBits // advance the packet reader past this bunch regardless of what we parse
		}
		p.processBunch(&b, br)
	}
}

func (p *Parser) processBunch(b *bunchHdr, br *bitReader) {
	defer func() {
		if e := recover(); e != nil {
			p.stats.panics++
		}
	}()
	if b.hasExports {
		p.receiveNetGUIDBunch(br)
	}
	ch := p.chans[b.chIndex]
	if ch == nil {
		ch = &Channel{Index: b.chIndex}
		p.chans[b.chIndex] = ch
	}
	if b.reliable {
		p.inReliable = b.chSeq
	}
	if b.partial {
		pb := p.partials[b.chIndex]
		if b.pInitial {
			pb = &partialBunch{chSeq: b.chSeq, reliable: b.reliable, open: b.open, hasExport: b.hasExports}
			p.partials[b.chIndex] = pb
			p.appendPartial(pb, br, b)
			return
		}
		if pb == nil {
			return
		}
		seqOK := b.chSeq == pb.chSeq+1
		if !pb.reliable {
			seqOK = seqOK || b.chSeq == pb.chSeq
		}
		if !seqOK || pb.reliable != b.reliable {
			delete(p.partials, b.chIndex)
			return
		}
		p.appendPartial(pb, br, b)
		pb.chSeq = b.chSeq
		if !b.pFinal {
			return
		}
		delete(p.partials, b.chIndex)
		whole := newBitReader(pb.data, pb.nbits)
		merged := *b
		merged.open = pb.open
		p.actorBunch(&merged, ch, whole)
		return
	}
	p.actorBunch(b, ch, br)
}

func (p *Parser) appendPartial(pb *partialBunch, br *bitReader, b *bunchHdr) {
	if b.hasExports {
		return
	}
	n := br.bitsLeft()
	for i := 0; i < n; i++ {
		bitv := br.bit()
		idx := pb.nbits + i
		if idx>>3 >= len(pb.data) {
			pb.data = append(pb.data, 0)
		}
		if bitv {
			pb.data[idx>>3] |= 1 << uint(idx&7)
		}
	}
	pb.nbits += n
}

func (p *Parser) actorBunch(b *bunchHdr, ch *Channel, br *bitReader) {
	if b.hasMustMap {
		n := int(br.u16())
		for i := 0; i < n; i++ {
			br.intPacked()
		}
	}
	p.contentBunch(b, ch, br)
	if b.close {
		if p.OnChanClose != nil && ch.opened {
			p.OnChanClose(p, ch)
		}
		delete(p.chans, b.chIndex)
	}
}

func (p *Parser) receiveNetGUIDBunch(r *bitReader) {
	if r.bit() { // has rep layout export (compat)
		n := r.u32()
		for i := uint32(0); i < n && !r.err; i++ {
			pathIndex := r.intPacked()
			var g *Group
			if r.bit() {
				path := r.fstring()
				num := r.u32()
				g = p.getOrCreateGroup(path, pathIndex, num)
			} else {
				g = p.groupByIndex[pathIndex]
			}
			e := p.readNetFieldExportBit(r)
			if g != nil && e != nil {
				g.set(e)
			}
		}
		return
	}
	n := int(r.i32())
	if n > 2048 || n < 0 {
		return
	}
	for i := 0; i < n && !r.err; i++ {
		p.internalLoadObject(r, true, 0)
	}
}

// contentBunch handles the spawn header of a new actor channel and then the content blocks.
func (p *Parser) contentBunch(b *bunchHdr, ch *Channel, br *bitReader) {
	if ch.Actor == 0 {
		if !b.open {
			return
		}
		ch.Actor = p.internalLoadObject(br, false, 0)
		if br.atEnd() && ch.Actor&1 == 0 {
			return
		}
		if ch.Actor > 0 && ch.Actor&1 == 0 { // dynamic actor: spawn info follows
			ch.Archetype = p.internalLoadObject(br, false, 0)
			p.internalLoadObject(br, false, 0) // level
			p.skipQuantVector(br)              // location
			if br.bit() {                      // rotation
				for i := 0; i < 3; i++ {
					if br.bit() {
						br.u16()
					}
				}
			}
			p.skipQuantVector(br) // scale
			p.skipQuantVector(br) // velocity
		}
		ch.opened = true
		if p.OnChanOpen != nil {
			p.OnChanOpen(p, ch)
		}
		if strings.Contains(p.guidPath[ch.Archetype], "ReplayPC") {
			br.u8() // net player index
		}
	}
	for !br.atEnd() && !br.err {
		repObj, hasRepLayout, deleted := p.contentBlockHeader(br, ch)
		if br.err {
			return
		}
		if deleted {
			continue
		}
		payload := int(br.intPacked())
		if br.err || payload > br.bitsLeft() {
			return
		}
		start := br.pos
		if repObj != 0 && hasRepLayout && payload > 0 {
			br.limit(payload)
			p.receiveProperties(br, ch, repObj)
			br.unlimit()
		}
		br.pos = start + payload
	}
}

func (p *Parser) skipQuantVector(r *bitReader) {
	if !r.bit() {
		return
	}
	if p.engVersion >= engOptionallyQuantize && !r.bit() {
		// not quantized: 3 doubles (LWC) or floats
		if p.engVersion >= engPackedVectorLWC {
			r.skip(64 * 3)
		} else {
			r.skip(32 * 3)
		}
		return
	}
	// quantized packed vector (scale 10, 24 bits legacy)
	if p.engVersion >= engPackedVectorLWC && p.engVersion != 26 {
		info := r.serializedInt(1 << 7)
		cnt := int(info & 63)
		extra := info >> 6
		if cnt > 0 {
			r.skip(cnt * 3)
		} else if extra == 0 {
			r.skip(32 * 3)
		} else {
			r.skip(64 * 3)
		}
		return
	}
	nb := r.serializedInt(24)
	mx := 1 << (nb + 2)
	r.serializedInt(mx)
	r.serializedInt(mx)
	r.serializedInt(mx)
}

func (p *Parser) contentBlockHeader(br *bitReader, ch *Channel) (repObj uint32, hasRepLayout bool, deleted bool) {
	hasRepLayout = br.bit()
	isActor := br.bit()
	if isActor {
		if ch.Archetype != 0 {
			return ch.Archetype, hasRepLayout, false
		}
		return ch.Actor, hasRepLayout, false
	}
	guid := p.internalLoadObject(br, false, 0)
	if br.bit() { // stably named sub-object
		return guid, hasRepLayout, false
	}
	deleteSub := false
	serializeClass := true
	if p.engVersion >= engSubobjectDestroyFlag {
		if br.bit() {
			deleteSub = true
			serializeClass = false
			br.u8()
		}
	}
	var classGUID uint32
	if serializeClass {
		classGUID = p.internalLoadObject(br, false, 0)
		deleteSub = classGUID == 0
	}
	if deleteSub {
		return 0, hasRepLayout, true
	}
	if p.engVersion >= engSubobjectOuterChain {
		actorIsOuter := br.atEnd() || br.bit()
		if !actorIsOuter {
			p.internalLoadObject(br, false, 0)
		}
	}
	return classGUID, hasRepLayout, false
}

// groupFor resolves the export group of a class/archetype net guid.
func (p *Parser) groupFor(guid uint32) *Group {
	if len(p.groups) != p.groupCacheN {
		p.groupCache = map[uint32]*Group{}
		p.groupCacheN = len(p.groups)
	}
	if g, ok := p.groupCache[guid]; ok {
		return g
	}
	path, ok := p.guidPath[guid]
	if !ok {
		return nil
	}
	name := strings.TrimPrefix(path, "Default__")
	var best *Group
	for _, g := range p.groups {
		if g.Fixed == name {
			best = g
			break
		}
	}
	if best == nil {
		for _, g := range p.groups {
			if strings.HasSuffix(g.Path, "ClassNetCache") {
				continue
			}
			if strings.Contains(path, g.Fixed) && (best == nil || len(g.Fixed) > len(best.Fixed)) {
				best = g
			}
		}
	}
	p.groupCache[guid] = best
	return best
}

// receiveProperties walks the rep-layout property list of a content block and decodes the few we care about.
func (p *Parser) receiveProperties(br *bitReader, ch *Channel, repObj uint32) {
	g := p.groupFor(repObj)
	if g == nil {
		return
	}
	br.bit() // property checksum flag
	var vals map[string]uint32
	for !br.atEnd() && !br.err {
		handle := br.intPacked()
		if handle == 0 {
			break
		}
		handle--
		nbits := int(br.intPacked())
		if br.err {
			break
		}
		if nbits == 0 {
			continue
		}
		if nbits > br.bitsLeft() {
			break
		}
		end := br.pos + nbits
		if int(handle) < len(g.Exports) && g.Exports[handle] != nil {
			switch kind := wantedProps[g.Exports[handle].Name]; kind {
			case kindBool:
				if vals == nil {
					vals = map[string]uint32{}
				}
				v := uint32(0)
				if br.bit() {
					v = 1
				}
				vals[g.Exports[handle].Name] = v
			case kindString:
				if nm := br.fstring(); nm != "" && !br.err {
					p.Names[ch.Actor] = nm
				}
			case kindGUID:
				if vals == nil {
					vals = map[string]uint32{}
				}
				vals[g.Exports[handle].Name] = br.intPacked()
			}
		}
		br.pos = end
		br.err = false
	}
	if vals != nil && p.OnProps != nil {
		p.OnProps(p, ch, g, vals)
	}
}

const (
	kindBool   = 1
	kindGUID   = 2
	kindString = 3
)

var wantedProps = map[string]int{
	"bAlreadySearched":    kindBool,
	"bDestroyed":          kindBool,
	"SearchingPawn":       kindGUID,
	"PlayerState":         kindGUID,
	"RecorderPlayerState": kindGUID,
	"PlayerNamePrivate":   kindString,
	"PlayerName":          kindString,
}
