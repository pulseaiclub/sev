package sev

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unsafe"
)

// Binary layout of specdb (little endian, header magic first):
//
//	[8]byte  magic "SEVDB\x00\x01\x00"
//	u32      stringCount
//	...      stringCount × (u32 len + len bytes) — the blob, offsets point here
//	u32      nodeCount
//	...      nodeCount × 8 u32: name, desc, firstChild, nChild, firstOpt, nOpt,
//	         gen, flags  (offsets are relative to the blob start)
//	u32      childCount
//	...      childCount × u32 (indices into nodes; a node's children are
//	         contiguous and sorted by name)
//	u32      optCount
//	...      optCount × 2 u32: name, desc
//	u32      root node index
//
// Strings are stored length-prefixed and never mutated, so str() hands out
// zero-copy views into the blob: a completion's name costs no allocation.
const specDBMagic = "SEVDB\x00\x01\x00"

// noStr marks an absent string/number in the format.
const noStr uint32 = 0xFFFFFFFF

// errorf keeps the loader's errors actionable: a truncated or foreign blob must
// say which section ran out, not just "unexpected EOF".
var ErrFormat = errors.New("sev: not a specdb blob")

// index is the compiled command tree. It is immutable after load and safe for
// concurrent use.
type index struct {
	blob     []byte
	nodes    []node
	children []uint32
	opts     []opt
	root     uint32
}

type node struct {
	name       uint32
	desc       uint32
	firstChild uint32
	nChild     uint32
	firstOpt   uint32
	nOpt       uint32
	gen        uint32
	flags      uint32
}

type opt struct {
	name uint32
	desc uint32
}

// str returns the string at a blob offset without copying. The blob outlives
// every index and is read-only, so the view stays valid.
func (ix *index) str(off uint32) string {
	// uint64 arithmetic: on a 32-bit build int(off)+4 can wrap and turn a
	// corrupt offset into a slice panic instead of an empty string.
	if off == noStr || uint64(off)+4 > uint64(len(ix.blob)) {
		return ""
	}
	n := uint64(binary.LittleEndian.Uint32(ix.blob[off:]))
	start := uint64(off) + 4
	if n == 0 {
		return ""
	}
	if start+n > uint64(len(ix.blob)) {
		return ""
	}
	return unsafe.String(&ix.blob[start], int(n))
}

func (ix *index) name(i uint32) string    { return ix.str(ix.nodes[i].name) }
func (ix *index) desc(i uint32) string    { return ix.str(ix.nodes[i].desc) }
func (ix *index) optName(i uint32) string { return ix.str(ix.opts[i].name) }
func (ix *index) optDesc(i uint32) string { return ix.str(ix.opts[i].desc) }

func (ix *index) genKey(i uint32) string {
	if ix.nodes[i].gen == noStr {
		return ""
	}
	return ix.str(ix.nodes[i].gen)
}

// childSlice returns the contiguous, name-sorted children of n.
func (ix *index) childSlice(n uint32) []uint32 {
	nd := &ix.nodes[n]
	if nd.nChild == 0 {
		return nil
	}
	return ix.children[nd.firstChild : nd.firstChild+nd.nChild]
}

func (ix *index) optSlice(n uint32) []opt {
	nd := &ix.nodes[n]
	if nd.nOpt == 0 {
		return nil
	}
	return ix.opts[nd.firstOpt : nd.firstOpt+nd.nOpt]
}

// child looks a child up by exact name: binary search over the sorted slice.
func (ix *index) child(n uint32, name string) (uint32, bool) {
	kids := ix.childSlice(n)
	lo, hi := 0, len(kids)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if ix.name(kids[mid]) < name {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(kids) && ix.name(kids[lo]) == name {
		return kids[lo], true
	}
	return 0, false
}

// childRange returns the run of children whose names start with prefix. Both
// bounds come from binary search, so the scan never visits unrelated commands —
// the names between "clone" and "config" include "commit", and a linear scan
// from the first hit would stop at the first name that does not match.
func (ix *index) childRange(n uint32, prefix string) []uint32 {
	kids := ix.childSlice(n)
	if prefix == "" || len(kids) == 0 {
		return kids
	}
	name := func(i uint32) string { return ix.name(i) }
	lo := lowerBound(kids, prefix, name)
	if lo == len(kids) || !hasPrefix(name(kids[lo]), prefix) {
		return nil
	}
	hi := len(kids)
	if end, ok := prefixUpperBound(prefix); ok {
		hi = lowerBound(kids, end, name)
	} else {
		hi = lo
		for hi < len(kids) && hasPrefix(name(kids[hi]), prefix) {
			hi++
		}
	}
	if hi < lo {
		return nil
	}
	return kids[lo:hi]
}

// prefixUpperBound is the smallest string greater than every string that starts
// with prefix: "co" -> "cp". Bytes are compared, and a Go string comparison is
// bytewise, so incrementing the last non-0xFF byte bounds the prefix range.
func prefixUpperBound(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

func lowerBound(kids []uint32, prefix string, name func(uint32) string) int {
	lo, hi := 0, len(kids)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if name(kids[mid]) < prefix {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

// loadGzip decodes a gzipped specdb blob. The decompressed bytes are kept as the
// string arena, so no decoding step copies a single command name.
func loadGzip(raw []byte) (*index, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("sev: open specdb: %w", err)
	}
	defer zr.Close()
	plain, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("sev: read specdb: %w", err)
	}
	return loadIndex(plain)
}

func loadIndex(raw []byte) (*index, error) {
	if len(raw) < len(specDBMagic) || string(raw[:len(specDBMagic)]) != specDBMagic {
		return nil, ErrFormat
	}
	c := cursor{buf: raw, off: len(specDBMagic)}

	blobLen := c.u32()
	if uint64(blobLen) > uint64(len(raw)) {
		return nil, c.fail("string blob")
	}
	blob, ok := c.bytes(int(blobLen))
	if !ok {
		return nil, c.fail("string blob")
	}

	// Allocate only what the buffer can back, in uint64: a corrupt count must not
	// become a multi-gigabyte make() the OS kills, nor a negative int on 32-bit.
	nodeCount := uint64(c.u32())
	if nodeCount == 0 || nodeCount > uint64(c.remaining()/nodeSize) {
		return nil, c.fail("node table")
	}
	nodes := make([]node, int(nodeCount))
	for i := range nodes {
		nd := &nodes[i]
		nd.name = c.u32()
		nd.desc = c.u32()
		nd.firstChild = c.u32()
		nd.nChild = c.u32()
		nd.firstOpt = c.u32()
		nd.nOpt = c.u32()
		nd.gen = c.u32()
		nd.flags = c.u32()
		if c.err != nil {
			return nil, c.fail("node table")
		}
	}

	childCount := uint64(c.u32())
	if childCount > uint64(c.remaining()/4) {
		return nil, c.fail("child table")
	}
	children := make([]uint32, int(childCount))
	for i := range children {
		children[i] = c.u32()
	}
	if c.err != nil {
		return nil, c.fail("child table")
	}

	optCount := uint64(c.u32())
	if optCount > uint64(c.remaining()/optSize) {
		return nil, c.fail("option table")
	}
	opts := make([]opt, int(optCount))
	for i := range opts {
		opts[i] = opt{name: c.u32(), desc: c.u32()}
	}
	if c.err != nil {
		return nil, c.fail("option table")
	}

	root := c.u32()
	if c.err != nil {
		return nil, c.fail("root node")
	}
	if uint64(root) >= nodeCount {
		return nil, fmt.Errorf("sev: root node %d out of range %d", root, nodeCount)
	}

	ix := &index{blob: blob, nodes: nodes, children: children, opts: opts, root: root}
	if err := ix.validate(); err != nil {
		return nil, err
	}
	return ix, nil
}

// validate rejects a blob whose slices point outside the tables. Without it a
// malformed blob would panic inside a completion on a keystroke, far from the
// load that caused it, so every node and every child index is checked once here.
func (ix *index) validate() error {
	var (
		nNodes = uint32(len(ix.nodes))
		nChild = uint32(len(ix.children))
		nOpts  = uint32(len(ix.opts))
	)
	for i := range ix.nodes {
		nd := &ix.nodes[i]
		if nd.nChild > 0 && uint64(nd.firstChild)+uint64(nd.nChild) > uint64(nChild) {
			return fmt.Errorf("sev: node %d children [%d,%d) exceed %d", i, nd.firstChild, nd.firstChild+nd.nChild, nChild)
		}
		if nd.nOpt > 0 && uint64(nd.firstOpt)+uint64(nd.nOpt) > uint64(nOpts) {
			return fmt.Errorf("sev: node %d options [%d,%d) exceed %d", i, nd.firstOpt, nd.firstOpt+nd.nOpt, nOpts)
		}
		if nd.nChild == 0 {
			continue
		}
		for _, kid := range ix.children[nd.firstChild : nd.firstChild+nd.nChild] {
			if kid >= nNodes {
				return fmt.Errorf("sev: node %d has child index %d, out of range %d", i, kid, nNodes)
			}
		}
	}
	return nil
}

type cursor struct {
	buf []byte
	off int
	err error
}

func (c *cursor) u32() uint32 {
	if c.err != nil {
		return 0
	}
	if c.off+4 > len(c.buf) {
		c.err = io.ErrUnexpectedEOF
		return 0
	}
	v := binary.LittleEndian.Uint32(c.buf[c.off:])
	c.off += 4
	return v
}

func (c *cursor) bytes(n int) ([]byte, bool) {
	if c.err != nil {
		return nil, false
	}
	if n < 0 || c.off+n > len(c.buf) {
		c.err = io.ErrUnexpectedEOF
		return nil, false
	}
	b := c.buf[c.off : c.off+n]
	c.off += n
	return b, true
}

// nodeSize and optSize are the encoded widths; the loader uses them to reject a
// count that the remaining bytes cannot possibly contain.
const (
	nodeSize = 32
	optSize  = 8
)

func (c *cursor) remaining() int {
	if c.err != nil || c.off >= len(c.buf) {
		return 0
	}
	return len(c.buf) - c.off
}

func (c *cursor) fail(section string) error {
	if c.err == nil {
		c.err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("sev: specdb %s truncated: %w", section, c.err)
}
