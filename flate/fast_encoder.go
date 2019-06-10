// Copyright 2011 The Snappy-Go Authors. All rights reserved.
// Modified for deflate by Klaus Post (c) 2015.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import "math/bits"

// emitLiteral writes a literal chunk and returns the number of bytes written.
func emitLiteral(dst *tokens, lit []byte) {
	ol := int(dst.n)
	for i, v := range lit {
		dst.tokens[(i+ol)&maxStoreBlockSize] = token(v)
	}
	dst.n += uint16(len(lit))
}

// emitCopy writes a copy chunk and returns the number of bytes written.
func emitCopy(dst *tokens, offset, length int) {
	dst.tokens[dst.n] = matchToken(uint32(length-3), uint32(offset-minOffsetSize))
	dst.n++
}

type fastEnc interface {
	Encode(dst *tokens, src []byte)
	Reset()
}

func newFastEnc(level int) fastEnc {
	switch level {
	case 1:
		return &fastEncL1{fastGen: fastGen{cur: maxStoreBlockSize}}
	case 2:
		return &fastEncL2{fastGen: fastGen{cur: maxStoreBlockSize}}
	case 3:
		return &fastEncL3{fastGen: fastGen{cur: maxStoreBlockSize}}
	case 4:
		return &fastEncL4{fastEncL3{fastGen: fastGen{cur: maxStoreBlockSize}}}
	default:
		panic("invalid level specified")
	}
}

const (
	tableBits       = 16             // Bits used in the table
	tableSize       = 1 << tableBits // Size of the table
	tableMask       = tableSize - 1  // Mask for table indices. Redundant, but can eliminate bounds checks.
	tableShift      = 32 - tableBits // Right-shift to get the tableBits most significant bits of a uint32.
	baseMatchOffset = 1              // The smallest match offset
	baseMatchLength = 3              // The smallest match length per the RFC section 3.2.5
	maxMatchOffset  = 1 << 15        // The largest match offset

	bTableBits = 18              // Bits used in the table
	bTableSize = 1 << bTableBits // Size of the table
	bTableMask = bTableSize - 1  // Mask for table indices. Redundant, but can eliminate bounds checks.

)

func load32(b []byte, i int) uint32 {
	// Help the compiler eliminate bounds checks on the read so it can be done in a single read.
	b = b[i:]
	b = b[:4]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func load64(b []byte, i int) uint64 {
	// Help the compiler eliminate bounds checks on the read so it can be done in a single read.
	b = b[i:]
	b = b[:8]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func load3232(b []byte, i int32) uint32 {
	// Help the compiler eliminate bounds checks on the read so it can be done in a single read.
	b = b[i:]
	b = b[:4]
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func load6432(b []byte, i int32) uint64 {
	// Help the compiler eliminate bounds checks on the read so it can be done in a single read.
	b = b[i:]
	b = b[:8]
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func hash(u uint32) uint32 {
	return (u * 0x1e35a7bd) >> tableShift
}

type tableEntry struct {
	val    uint32
	offset int32
}

// fastGen maintains the table for matches,
// and the previous byte block for level 2.
// This is the generic implementation.
type fastGen struct {
	hist []byte
	cur  int32
}

func (e *fastGen) addBlock(src []byte) int32 {
	// check if we have space already
	if len(e.hist)+len(src) > cap(e.hist) {
		if cap(e.hist) == 0 {
			l := maxMatchOffset * 10
			e.hist = make([]byte, 0, l)
		} else {
			if cap(e.hist) < int(maxMatchOffset*2) {
				panic("unexpected buffer size")
			}
			// Move down
			offset := int32(len(e.hist)) - maxMatchOffset
			copy(e.hist[0:maxMatchOffset], e.hist[offset:])
			e.cur += offset
			e.hist = e.hist[:maxMatchOffset]
		}
	}
	s := int32(len(e.hist))
	e.hist = append(e.hist, src...)
	return s
}

// fastGen maintains the table for matches,
// and the previous byte block for level 2.
// This is the generic implementation.
type fastEncL1 struct {
	fastGen
	table [tableSize]tableEntry
}

// EncodeL1 uses a similar algorithm to level 1
func (e *fastEncL1) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 12 - 1
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// Protect against e.cur wraparound.
	for e.cur >= (1<<31)-(maxStoreBlockSize*2) {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntry{}
			}
			e.cur = maxMatchOffset
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - maxMatchOffset
		for i := range e.table[:] {
			v := e.table[i].offset
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + maxMatchOffset
			}
			e.table[i].offset = v
		}
		e.cur = maxMatchOffset
	}

	s := e.addBlock(src)

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		return
	}

	// Override src
	src = e.hist
	nextEmit := s

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := int32(len(src) - inputMargin)

	// nextEmit is where in src the next emitLiteral should start from.
	cv := load3232(src, s)
	nextHash := hash(cv)

	for {
		const skipLog = 6
		const doEvery = 2

		nextS := s
		var candidate tableEntry
		for {
			s = nextS
			nextS = s + doEvery + (s-nextEmit)>>skipLog
			if nextS > sLimit {
				goto emitRemainder
			}
			candidate = e.table[nextHash&tableMask]
			now := load6432(src, nextS)
			e.table[nextHash&tableMask] = tableEntry{offset: s + e.cur, val: cv}
			nextHash = hash(uint32(now))

			offset := s - (candidate.offset - e.cur)
			if offset < maxMatchOffset && cv == candidate.val {
				break
			}

			// Do one right away...
			cv = uint32(now)
			s = nextS
			nextS++
			candidate = e.table[nextHash&tableMask]
			now >>= 8
			e.table[nextHash&tableMask] = tableEntry{offset: s + e.cur, val: cv}
			nextHash = hash(uint32(now))

			offset = s - (candidate.offset - e.cur)
			if offset < maxMatchOffset && cv == candidate.val {
				break
			}
			cv = uint32(now)
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.

			// Extend the 4-byte match as long as possible.
			t := candidate.offset - e.cur
			l := e.matchlen(s+4, t+4, src) + 4

			// Extend backwards
			tMin := s - maxMatchOffset
			if tMin < 0 {
				tMin = 0
			}
			for t > tMin && s > nextEmit && src[t-1] == src[s-1] && l < maxMatchLength {
				s--
				t--
				l++
			}
			if nextEmit < s {
				emitLiteral(dst, src[nextEmit:s])
			}

			// matchToken is flate's equivalent of Snappy's emitCopy. (length,offset)
			dst.tokens[dst.n] = matchToken(uint32(l-baseMatchLength), uint32(s-t-baseMatchOffset))
			dst.n++
			s += l
			nextEmit = s
			if s >= sLimit {
				// Index first pair after match end.
				if int(s+l+4) < len(src) {
					cv := load3232(src, s)
					e.table[hash(cv)&tableMask] = tableEntry{offset: s + e.cur, val: cv}
				}
				goto emitRemainder
			}

			// Store every second hash in-between, but offset by 1.
			if false {
				for i := s - l - 2; i < s-7; i += 7 {
					x := load6432(src, int32(i))
					nextHash := hash(uint32(x))
					e.table[nextHash&tableMask] = tableEntry{offset: e.cur + i, val: uint32(x)}
					// Skip one
					x >>= 16
					nextHash = hash(uint32(x))
					e.table[nextHash&tableMask] = tableEntry{offset: e.cur + i + 2, val: uint32(x)}
					// Skip one
					x >>= 16
					nextHash = hash(uint32(x))
					e.table[nextHash&tableMask] = tableEntry{offset: e.cur + i + 4, val: uint32(x)}
				}
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load6432(src, s-2)
			o := e.cur + s - 2
			prevHash := hash(uint32(x))
			prevHash2 := hash(uint32(x >> 8))
			e.table[prevHash&tableMask] = tableEntry{offset: o, val: uint32(x)}
			e.table[prevHash2&tableMask] = tableEntry{offset: o + 1, val: uint32(x >> 8)}
			currHash := hash(uint32(x >> 16))
			candidate = e.table[currHash&tableMask]
			e.table[currHash&tableMask] = tableEntry{offset: o + 2, val: uint32(x >> 16)}

			offset := s - (candidate.offset - e.cur)
			if offset > maxMatchOffset || uint32(x>>16) != candidate.val {
				cv = uint32(x >> 24)
				nextHash = hash(cv)
				s++
				break
			}
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
}

// fastGen maintains the table for matches,
// and the previous byte block for level 2.
// This is the generic implementation.
type fastEncL2 struct {
	fastGen
	table [bTableSize]tableEntry
}

// hash4 returns the hash of u to fit in a hash table with h bits.
// Preferably h should be a constant and should always be <32.
func hash4u(u uint32, h uint8) uint32 {
	const prime4bytes = 2654435761
	return (u * prime4bytes) >> ((32 - h) & 31)
}

// EncodeL2 uses a similar algorithm to level 1, but is capable
// of matching across blocks giving better compression at a small slowdown.
func (e *fastEncL2) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 12 - 1
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// Protect against e.cur wraparound.
	for e.cur >= (1<<31)-(maxStoreBlockSize*2) {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntry{}
			}
			e.cur = maxMatchOffset
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - maxMatchOffset
		for i := range e.table[:] {
			v := e.table[i].offset
			if v < minOff {
				v = 0
			} else {
				v = v - e.cur + maxMatchOffset
			}
			e.table[i].offset = v
		}
		e.cur = maxMatchOffset
	}

	s := e.addBlock(src)

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		return
	}

	// Override src
	src = e.hist
	nextEmit := s

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := int32(len(src) - inputMargin)

	// nextEmit is where in src the next emitLiteral should start from.
	cv := load3232(src, s)
	nextHash := hash4u(cv, bTableBits)

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		const skipLog = 6
		const doEvery = 2

		nextS := s
		var candidate tableEntry
		for {
			s = nextS
			nextS = s + doEvery + (s-nextEmit)>>skipLog
			if nextS > sLimit {
				goto emitRemainder
			}
			candidate = e.table[nextHash&bTableMask]
			now := load6432(src, nextS)
			e.table[nextHash&bTableMask] = tableEntry{offset: s + e.cur, val: cv}
			nextHash = hash4u(uint32(now), bTableBits)

			offset := s - (candidate.offset - e.cur)
			if offset < maxMatchOffset && cv == candidate.val {
				break
			}

			// Do one right away...
			cv = uint32(now)
			s = nextS
			nextS++
			candidate = e.table[nextHash&bTableMask]
			now >>= 8
			e.table[nextHash&bTableMask] = tableEntry{offset: s + e.cur, val: cv}
			nextHash = hash4u(uint32(now), bTableBits)

			offset = s - (candidate.offset - e.cur)
			if offset < maxMatchOffset && cv == candidate.val {
				break
			}
			cv = uint32(now)
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.

			// Extend the 4-byte match as long as possible.
			t := candidate.offset - e.cur
			l := e.matchlen(s+4, t+4, src) + 4

			// Extend backwards
			tMin := s - maxMatchOffset
			if tMin < 0 {
				tMin = 0
			}
			for t > tMin && s > nextEmit && src[t-1] == src[s-1] && l < maxMatchLength {
				s--
				t--
				l++
			}
			if nextEmit < s {
				emitLiteral(dst, src[nextEmit:s])
			}

			// matchToken is flate's equivalent of Snappy's emitCopy. (length,offset)
			dst.tokens[dst.n] = matchToken(uint32(l-baseMatchLength), uint32(s-t-baseMatchOffset))
			dst.n++
			s += l
			nextEmit = s
			if s >= sLimit {
				// Index first pair after match end.
				if int(s+l+4) < len(src) {
					cv := load3232(src, s)
					e.table[hash4u(cv, bTableBits)&bTableMask] = tableEntry{offset: s + e.cur, val: cv}
				}
				goto emitRemainder
			}

			// Store every second hash in-between, but offset by 1.
			if true {
				for i := s - l - 2; i < s-7; i += 7 {
					x := load6432(src, int32(i))
					nextHash := hash4u(uint32(x), bTableBits)
					e.table[nextHash&bTableMask] = tableEntry{offset: e.cur + i, val: uint32(x)}
					// Skip one
					x >>= 16
					nextHash = hash4u(uint32(x), bTableBits)
					e.table[nextHash&bTableMask] = tableEntry{offset: e.cur + i + 2, val: uint32(x)}
					// Skip one
					x >>= 16
					nextHash = hash4u(uint32(x), bTableBits)
					e.table[nextHash&bTableMask] = tableEntry{offset: e.cur + i + 4, val: uint32(x)}
				}
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-1 and at s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load6432(src, s-2)
			o := e.cur + s - 2
			prevHash := hash4u(uint32(x), bTableBits)
			prevHash2 := hash4u(uint32(x>>8), bTableBits)
			e.table[prevHash&bTableMask] = tableEntry{offset: o, val: uint32(x)}
			e.table[prevHash2&bTableMask] = tableEntry{offset: o + 1, val: uint32(x >> 8)}
			currHash := hash4u(uint32(x>>16), bTableBits)
			candidate = e.table[currHash&bTableMask]
			e.table[currHash&bTableMask] = tableEntry{offset: o + 2, val: uint32(x >> 16)}

			offset := s - (candidate.offset - e.cur)
			if offset > maxMatchOffset || uint32(x>>16) != candidate.val {
				cv = uint32(x >> 24)
				nextHash = hash4u(uint32(cv), bTableBits)
				s++
				break
			}
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
}

type tableEntryPrev struct {
	Cur  tableEntry
	Prev tableEntry
}

// fastEncL3
type fastEncL3 struct {
	fastGen
	table [tableSize]tableEntryPrev
}

// Encode uses a similar algorithm to level 2, will check up to two candidates.
func (e *fastEncL3) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 8 - 1
		minNonLiteralBlockSize = 1 + 1 + inputMargin
	)

	// Protect against e.cur wraparound.
	for e.cur >= (1<<31)-(maxStoreBlockSize) {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntryPrev{}
			}
			e.cur = maxMatchOffset
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - maxMatchOffset
		for i := range e.table[:] {
			v := e.table[i]
			if v.Cur.offset < minOff {
				v.Cur.offset = 0
			} else {
				v.Cur.offset = v.Cur.offset - e.cur + maxMatchOffset
			}
			if v.Prev.offset < minOff {
				v.Prev.offset = 0
			} else {
				v.Prev.offset = v.Prev.offset - e.cur + maxMatchOffset
			}
			e.table[i] = v
		}
		e.cur = maxMatchOffset
	}

	s := e.addBlock(src)

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		return
	}

	// Override src
	src = e.hist
	nextEmit := s

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := int32(len(src) - inputMargin)

	// nextEmit is where in src the next emitLiteral should start from.
	cv := load3232(src, s)
	nextHash := hash(cv)

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := int32(32)

		nextS := s
		var candidate tableEntry
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidates := e.table[nextHash&tableMask]
			now := load3232(src, nextS)
			e.table[nextHash&tableMask] = tableEntryPrev{Prev: candidates.Cur, Cur: tableEntry{offset: s + e.cur, val: cv}}
			nextHash = hash(now)

			// Check both candidates
			candidate = candidates.Cur
			if cv == candidate.val {
				offset := s - (candidate.offset - e.cur)
				if offset <= maxMatchOffset {
					break
				}
			} else {
				// We only check if value mismatches.
				// Offset will always be invalid in other cases.
				candidate = candidates.Prev
				if cv == candidate.val {
					offset := s - (candidate.offset - e.cur)
					if offset <= maxMatchOffset {
						break
					}
				}
			}
			cv = now
		}

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.

			// Extend the 4-byte match as long as possible.
			//
			t := candidate.offset - e.cur
			l := e.matchlen(s+4, t+4, src) + 4

			// Extend backwards
			tMin := s - maxMatchOffset
			if tMin < 0 {
				tMin = 0
			}
			for t > tMin && s > nextEmit && src[t-1] == src[s-1] && l < maxMatchLength {
				s--
				t--
				l++
			}
			if nextEmit < s {
				emitLiteral(dst, src[nextEmit:s])
			}

			// matchToken is flate's equivalent of Snappy's emitCopy. (length,offset)
			dst.tokens[dst.n] = matchToken(uint32(l-baseMatchLength), uint32(s-t-baseMatchOffset))
			dst.n++
			s += l
			nextEmit = s
			if s >= sLimit {
				t += l
				// Index first pair after match end.
				if int(t+4) < len(src) && t > 0 {
					cv := load3232(src, t)
					nextHash = hash(cv)
					e.table[nextHash&tableMask] = tableEntryPrev{
						Prev: e.table[nextHash&tableMask].Cur,
						Cur:  tableEntry{offset: e.cur + t, val: cv},
					}
				}
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-3 to s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load6432(src, s-3)
			prevHash := hash(uint32(x))
			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 3, val: uint32(x)},
			}
			x >>= 8
			prevHash = hash(uint32(x))

			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 2, val: uint32(x)},
			}
			x >>= 8
			prevHash = hash(uint32(x))

			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 1, val: uint32(x)},
			}
			x >>= 8
			currHash := hash(uint32(x))
			candidates := e.table[currHash&tableMask]
			cv = uint32(x)
			e.table[currHash&tableMask] = tableEntryPrev{
				Prev: candidates.Cur,
				Cur:  tableEntry{offset: s + e.cur, val: cv},
			}

			// Check both candidates
			candidate = candidates.Cur
			if cv == candidate.val {
				offset := s - (candidate.offset - e.cur)
				if offset <= maxMatchOffset {
					continue
				}
			} else {
				// We only check if value mismatches.
				// Offset will always be invalid in other cases.
				candidate = candidates.Prev
				if cv == candidate.val {
					offset := s - (candidate.offset - e.cur)
					if offset <= maxMatchOffset {
						continue
					}
				}
			}
			cv = uint32(x >> 8)
			nextHash = hash(cv)
			s++
			break
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
}

// fastEncL4
type fastEncL4 struct {
	fastEncL3
}

// Encode uses a similar algorithm to level 3,
// but will check up to two candidates if first isn't long enough.
func (e *fastEncL4) Encode(dst *tokens, src []byte) {
	const (
		inputMargin            = 8 - 3
		minNonLiteralBlockSize = 1 + 1 + inputMargin
		matchLenGood           = 12
	)

	// Protect against e.cur wraparound.
	for e.cur >= (1<<31)-(maxStoreBlockSize) {
		if len(e.hist) == 0 {
			for i := range e.table[:] {
				e.table[i] = tableEntryPrev{}
			}
			e.cur = maxMatchOffset
			break
		}
		// Shift down everything in the table that isn't already too far away.
		minOff := e.cur + int32(len(e.hist)) - maxMatchOffset
		for i := range e.table[:] {
			v := e.table[i]
			if v.Cur.offset < minOff {
				v.Cur.offset = 0
			} else {
				v.Cur.offset = v.Cur.offset - e.cur + maxMatchOffset
			}
			if v.Prev.offset < minOff {
				v.Prev.offset = 0
			} else {
				v.Prev.offset = v.Prev.offset - e.cur + maxMatchOffset
			}
			e.table[i] = v
		}
		e.cur = maxMatchOffset
	}

	s := e.addBlock(src)

	// This check isn't in the Snappy implementation, but there, the caller
	// instead of the callee handles this case.
	if len(src) < minNonLiteralBlockSize {
		// We do not fill the token table.
		// This will be picked up by caller.
		dst.n = uint16(len(src))
		return
	}

	// Override src
	src = e.hist
	nextEmit := s

	// sLimit is when to stop looking for offset/length copies. The inputMargin
	// lets us use a fast path for emitLiteral in the main loop, while we are
	// looking for copies.
	sLimit := int32(len(src) - inputMargin)

	// nextEmit is where in src the next emitLiteral should start from.
	cv := load3232(src, s)
	nextHash := hash(cv)

	for {
		// Copied from the C++ snappy implementation:
		//
		// Heuristic match skipping: If 32 bytes are scanned with no matches
		// found, start looking only at every other byte. If 32 more bytes are
		// scanned (or skipped), look at every third byte, etc.. When a match
		// is found, immediately go back to looking at every byte. This is a
		// small loss (~5% performance, ~0.1% density) for compressible data
		// due to more bookkeeping, but for non-compressible data (such as
		// JPEG) it's a huge win since the compressor quickly "realizes" the
		// data is incompressible and doesn't bother looking for matches
		// everywhere.
		//
		// The "skip" variable keeps track of how many bytes there are since
		// the last match; dividing it by 32 (ie. right-shifting by five) gives
		// the number of bytes to move ahead for each iteration.
		skip := int32(32)

		nextS := s
		var candidate tableEntry
		var candidateAlt tableEntry
		for {
			s = nextS
			bytesBetweenHashLookups := skip >> 5
			nextS = s + bytesBetweenHashLookups
			skip += bytesBetweenHashLookups
			if nextS > sLimit {
				goto emitRemainder
			}
			candidates := e.table[nextHash&tableMask]
			now := load3232(src, nextS)
			e.table[nextHash&tableMask] = tableEntryPrev{Prev: candidates.Cur, Cur: tableEntry{offset: s + e.cur, val: cv}}
			nextHash = hash(now)

			// Check both candidates
			candidate = candidates.Cur
			if cv == candidate.val {
				offset := s - (candidate.offset - e.cur)
				if offset < maxMatchOffset {
					offset = s - (candidates.Prev.offset - e.cur)
					if cv == candidates.Prev.val && offset < maxMatchOffset {
						candidateAlt = candidates.Prev
					}
					break
				}
			} else {
				// We only check if value mismatches.
				// Offset will always be invalid in other cases.
				candidate = candidates.Prev
				if cv == candidate.val {
					offset := s - (candidate.offset - e.cur)
					if offset < maxMatchOffset {
						break
					}
				}
			}
			cv = now
		}

		// A 4-byte match has been found. We'll later see if more than 4 bytes
		// match. But, prior to the match, src[nextEmit:s] are unmatched. Emit
		// them as literal bytes.
		emitLiteral(dst, src[nextEmit:s])

		// Call emitCopy, and then see if another emitCopy could be our next
		// move. Repeat until we find no match for the input immediately after
		// what was consumed by the last emitCopy call.
		//
		// If we exit this loop normally then we need to call emitLiteral next,
		// though we don't yet know how big the literal will be. We handle that
		// by proceeding to the next iteration of the main loop. We also can
		// exit this loop via goto if we get close to exhausting the input.
		for {
			// Invariant: we have a 4-byte match at s, and no need to emit any
			// literal bytes prior to s.

			// Extend the 4-byte match as long as possible.
			//
			s += 4
			t := candidate.offset - e.cur + 4
			l := e.matchlen(s, t, src)
			// Try alternative candidate if match length < matchLenGood.
			if l < matchLenGood-4 && candidateAlt.offset != 0 {
				t2 := candidateAlt.offset - e.cur + 4
				l2 := e.matchlen(s, t2, src)
				if l2 > l {
					l = l2
					t = t2
				}
			}
			// matchToken is flate's equivalent of Snappy's emitCopy. (length,offset)
			dst.tokens[dst.n] = matchToken(uint32(l+4-baseMatchLength), uint32(s-t-baseMatchOffset))
			dst.n++
			s += l
			nextEmit = s
			if s >= sLimit {
				t += l
				// Index first pair after match end.
				if int(t+4) < len(src) && t > 0 {
					cv := load3232(src, t)
					nextHash = hash(cv)
					e.table[nextHash&tableMask] = tableEntryPrev{
						Prev: e.table[nextHash&tableMask].Cur,
						Cur:  tableEntry{offset: e.cur + t, val: cv},
					}
				}
				goto emitRemainder
			}

			// We could immediately start working at s now, but to improve
			// compression we first update the hash table at s-3 to s. If
			// another emitCopy is not our next move, also calculate nextHash
			// at s+1. At least on GOARCH=amd64, these three hash calculations
			// are faster as one load64 call (with some shifts) instead of
			// three load32 calls.
			x := load6432(src, s-3)
			prevHash := hash(uint32(x))
			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 3, val: uint32(x)},
			}
			x >>= 8
			prevHash = hash(uint32(x))

			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 2, val: uint32(x)},
			}
			x >>= 8
			prevHash = hash(uint32(x))

			e.table[prevHash&tableMask] = tableEntryPrev{
				Prev: e.table[prevHash&tableMask].Cur,
				Cur:  tableEntry{offset: e.cur + s - 1, val: uint32(x)},
			}
			x >>= 8
			currHash := hash(uint32(x))
			candidates := e.table[currHash&tableMask]
			cv = uint32(x)
			e.table[currHash&tableMask] = tableEntryPrev{
				Prev: candidates.Cur,
				Cur:  tableEntry{offset: s + e.cur, val: cv},
			}

			// Check both candidates
			candidate = candidates.Cur
			candidateAlt = tableEntry{}
			if cv == candidate.val {
				offset := s - (candidate.offset - e.cur)
				if offset <= maxMatchOffset {
					offset = s - (candidates.Prev.offset - e.cur)
					if cv == candidates.Prev.val && offset <= maxMatchOffset {
						candidateAlt = candidates.Prev
					}
					continue
				}
			} else {
				// We only check if value mismatches.
				// Offset will always be invalid in other cases.
				candidate = candidates.Prev
				if cv == candidate.val {
					offset := s - (candidate.offset - e.cur)
					if offset <= maxMatchOffset {
						continue
					}
				}
			}
			cv = uint32(x >> 8)
			nextHash = hash(cv)
			s++
			break
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		emitLiteral(dst, src[nextEmit:])
	}
}

func (e *fastGen) matchlen(s, t int32, src []byte) int32 {
	s1 := int(s) + maxMatchLength - 4
	if s1 > len(src) {
		s1 = len(src)
	}

	// Extend the match to be as long as possible.
	return int32(matchLen(src[s:s1], src[t:]))
}

// Reset the encoding table.
func (e *fastGen) Reset() {
	if cap(e.hist) < int(maxMatchOffset*2) {
		l := maxMatchOffset * 2
		// Make it at least 1MB.
		if l < 1<<20 {
			l = 1 << 20
		}
		e.hist = make([]byte, 0, l)
	}
	// We offset current position so everything will be out of reach
	e.cur += maxMatchOffset + int32(len(e.hist))
	e.hist = e.hist[:0]
}

// matchLen returns the maximum length.
// 'a' must be the shortest of the two.
func matchLen(a, b []byte) int {
	b = b[:len(a)]
	var checked int
	if len(a) > 4 {
		// Try 4 bytes first
		if diff := load32(a, 0) ^ load32(b, 0); diff != 0 {
			return bits.TrailingZeros32(diff) >> 3
		}
		// Switch to 8 byte matching.
		for i := 4; i < len(a)-7; i += 8 {
			if diff := load64(a, i) ^ load64(b, i); diff != 0 {
				return i + (bits.TrailingZeros64(diff) >> 3)
			}
		}
		checked = 4 + ((len(a)-4)>>3)<<3
		a = a[checked:]
		b = b[checked:]
	}
	for i := range a {
		if a[i] != b[i] {
			return int(i) + checked
		}
	}
	return len(a) + checked
}