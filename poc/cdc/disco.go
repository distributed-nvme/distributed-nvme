package main

import (
	"encoding/binary"
)

const (
	trtypeTCP  = 2
	adrfamIPv4 = 1
	subtypeIO  = 1
	treqNone   = 0
	cntlidDyn  = 0xffff
	asqsz32    = 32
)

func buildDiscLog(m *model, hostnqn string, genctr uint64) []byte {
	var entries [][]byte
	portid := uint16(1)
	for i := range m.Subsystems {
		s := &m.Subsystems[i]
		if !hostAllowed(s, hostnqn) {
			continue
		}
		for _, p := range s.Ports {
			entries = append(entries, buildDiscEntry(s.Nqn, p.Traddr, p.Trsvcid, portid))
			portid++
		}
	}
	total := discLogHdrSize + len(entries)*discLogEntrySize
	page := make([]byte, total)
	binary.LittleEndian.PutUint64(page[0:8], genctr)
	binary.LittleEndian.PutUint64(page[8:16], uint64(len(entries)))
	binary.LittleEndian.PutUint16(page[16:18], 0)
	for i, e := range entries {
		copy(page[discLogHdrSize+i*discLogEntrySize:], e)
	}
	return page
}

func buildDiscEntry(nqn, traddr, trsvcid string, portid uint16) []byte {
	e := make([]byte, discLogEntrySize)
	e[0] = trtypeTCP
	e[1] = adrfamIPv4
	e[2] = subtypeIO
	e[3] = treqNone
	binary.LittleEndian.PutUint16(e[4:6], portid)
	binary.LittleEndian.PutUint16(e[6:8], cntlidDyn)
	binary.LittleEndian.PutUint16(e[8:10], asqsz32)
	binary.LittleEndian.PutUint16(e[10:12], 0)
	copy(e[0x20:], zeroPad(trsvcid, 32))
	copy(e[0x100:], zeroPad(nqn, 256))
	copy(e[0x200:], zeroPad(traddr, 256))
	return e
}

func zeroPad(s string, w int) []byte {
	b := make([]byte, w)
	n := len(s)
	if n > w {
		n = w
	}
	copy(b, s[:n])
	return b
}

func buildIdentifyCtrl() [identifyDataSize]byte {
	var id [identifyDataSize]byte
	id[0x4C] = 0
	id[0x4D] = 0
	binary.LittleEndian.PutUint16(id[0x4E:0x50], 0xffff)
	binary.LittleEndian.PutUint32(id[0x5C:0x60], 1<<31)
	id[0x6F] = 0x02
	binary.LittleEndian.PutUint16(id[0x100:0x102], 0)
	id[0x103] = 4
	id[0x105] = 1 << 2
	binary.LittleEndian.PutUint16(id[0x140:0x142], 1)
	id[0x200] = 0x66
	id[0x201] = 0x44
	binary.LittleEndian.PutUint16(id[0x202:0x204], 32)
	binary.LittleEndian.PutUint32(id[0x204:0x208], 0)
	binary.LittleEndian.PutUint16(id[0x208:0x20A], 0)
	binary.LittleEndian.PutUint32(id[0x216:0x21A], 1)
	binary.LittleEndian.PutUint32(id[0x21A:0x21E], 0)
	copy(id[0x300:], zeroPad(discNQN, 256))
	id[0x70E] = 0x02
	return id
}
