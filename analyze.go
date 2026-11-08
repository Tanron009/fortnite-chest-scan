package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

type MatchResult struct {
	File       string
	LengthSec  int
	Recorder   string
	Chests     int
	Ammo       int
	Other      int
	Destroyed  int
	PerPlayer  map[string]int // chests per player name (debug view)
	Frames     int
	PacketErrs int
}

func classify(label string) string {
	l := strings.ToLower(label)
	switch {
	case strings.Contains(l, "ammo"):
		return "ammo"
	case strings.Contains(l, "cannon"), strings.Contains(l, "foodbox"), strings.Contains(l, "food"):
		return "other"
	case strings.Contains(l, "chest"):
		return "chest"
	}
	return "other"
}

func analyzeReplay(path, wantName string) (res *MatchResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("unexpected error: %v", r)
		}
	}()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &byteReader{b: raw}
	info, err := parseFileInfo(r)
	if err != nil {
		return nil, fmt.Errorf("not a readable replay: %w", err)
	}
	if info.IsLive {
		return nil, fmt.Errorf("replay is still being recorded")
	}
	chunks, err := readChunks(r)
	if err != nil && len(chunks) == 0 {
		return nil, err
	}
	p := NewParser()
	haveHeader := false
	for _, c := range chunks {
		if c.Type == chunkHeader {
			if err := p.ReadHeader(c.Data); err != nil {
				return nil, err
			}
			haveHeader = true
		}
	}
	if !haveHeader {
		return nil, fmt.Errorf("replay has no header chunk")
	}

	psOf := map[uint32]uint32{} // pawn channel actor -> player state guid
	var recorder uint32
	searcherOf := map[uint32]uint32{}
	type ev struct {
		actor     uint32
		label     string
		destroyed bool
	}
	var events []ev
	seen := map[uint32]bool{}
	p.OnProps = func(p *Parser, ch *Channel, g *Group, v map[string]uint32) {
		if ps, ok := v["PlayerState"]; ok && ps != 0 {
			psOf[ch.Actor] = ps
		}
		if rs, ok := v["RecorderPlayerState"]; ok && rs != 0 && recorder == 0 {
			recorder = rs
		}
		if !g.hasName["bAlreadySearched"] {
			return
		}
		if s, ok := v["SearchingPawn"]; ok && s != 0 {
			searcherOf[ch.Actor] = s
		}
		if seen[ch.Actor] {
			return
		}
		id := ch.Archetype
		if id == 0 {
			id = ch.Actor
		}
		label := strings.TrimPrefix(p.guidPath[id], "Default__")
		if v["bAlreadySearched"] == 1 {
			seen[ch.Actor] = true
			events = append(events, ev{ch.Actor, label, false})
		} else if v["bDestroyed"] == 1 && searcherOf[ch.Actor] != 0 {
			seen[ch.Actor] = true
			events = append(events, ev{ch.Actor, label, true})
		}
	}
	chunkErrs := 0
	for _, c := range chunks {
		if c.Type != chunkReplayData {
			continue
		}
		d, err := dataChunk(info, c)
		if err != nil {
			chunkErrs++
			if chunkErrs == 1 {
				err2 := err
				_ = err2
			}
			continue
		}
		p.ReadChunkData(d)
	}
	if p.stats.frames == 0 {
		return nil, fmt.Errorf("could not read any game data (unsupported game build?)")
	}

	// decide which player state to count
	target := recorder
	if wantName != "" {
		for guid, nm := range p.Names {
			if strings.EqualFold(nm, wantName) {
				target = guid
			}
		}
	}
	res = &MatchResult{File: path, LengthSec: int(info.LengthMs / 1000), PerPlayer: map[string]int{},
		Recorder: p.Names[recorder], Frames: p.stats.frames, PacketErrs: p.stats.panics}
	for _, e := range events {
		pawn := searcherOf[e.actor]
		if pawn == 0 {
			continue
		}
		ps := psOf[pawn]
		kind := classify(e.label)
		if kind == "chest" {
			nm := p.Names[ps]
			if nm == "" {
				nm = fmt.Sprintf("player#%d", ps)
			}
			res.PerPlayer[nm]++
		}
		if target == 0 || ps != target {
			continue
		}
		switch kind {
		case "chest":
			res.Chests++
			if e.destroyed {
				res.Destroyed++
			}
		case "ammo":
			res.Ammo++
		default:
			res.Other++
		}
	}
	if chunkErrs > 0 {
		return res, fmt.Errorf("%d data chunk(s) could not be decoded, count may be low", chunkErrs)
	}
	return res, nil
}

func topPlayers(m map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].v != l[j].v {
			return l[i].v > l[j].v
		}
		return l[i].k < l[j].k
	})
	var out []string
	for i := 0; i < len(l) && i < n; i++ {
		out = append(out, fmt.Sprintf("%s=%d", l[i].k, l[i].v))
	}
	return out
}
