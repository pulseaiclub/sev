// Command genspecs compiles the checked-in command corpus into the specdb binary
// the sev engine embeds.
//
// The corpus is a plain JSON tree (tools/genspecs/corpus/commands.json.gz): names,
// descriptions, flags, and the generator key for argument positions. Nothing here
// reaches outside this repository — the tool needs the standard library and the
// corpus file, so rebuilding the data never depends on someone else's module.
//
// Usage:
//
//	go run . -out ../../specdb.bin.gz
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
)

const (
	magic = "SEVDB\x00\x01\x00"
	noStr = ^uint32(0)
)

// jnode is the on-disk corpus shape. Short keys because the file is 573 commands
// deep and readability lives in the tree, not the labels.
type jnode struct {
	Name string  `json:"n"`
	Desc string  `json:"d,omitempty"`
	Gen  string  `json:"g,omitempty"`
	Opts []jopt  `json:"o,omitempty"`
	Kids []jnode `json:"c,omitempty"`
}

type jopt struct {
	Name string `json:"n"`
	Desc string `json:"d,omitempty"`
}

func main() {
	src := flag.String("src", "corpus/commands.json.gz", "command corpus to compile")
	out := flag.String("out", "../../specdb.bin.gz", "output path for the gzipped specdb")
	verbose := flag.Bool("v", false, "print per-command stats")
	flag.Parse()

	roots := loadCorpus(*src)
	ix := flatten(roots)
	blob, err := encode(ix)
	if err != nil {
		fail("encode: %v", err)
	}

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		fail("gzip: %v", err)
	}
	if _, err := zw.Write(blob); err != nil {
		fail("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		fail("gzip close: %v", err)
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		fail("write %s: %v", *out, err)
	}

	genCount := map[string]int{}
	for i := range ix.nodes {
		if g := ix.nodes[i].gen; g != "" {
			genCount[g]++
		}
	}
	keys := make([]string, 0, len(genCount))
	for k := range genCount {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Printf("commands=%d nodes=%d blob=%dKiB gz=%dKiB out=%s\n",
		len(roots), len(ix.nodes), len(ix.blob)/1024, buf.Len()/1024, *out)
	for _, k := range keys {
		fmt.Printf("  generator %-12s %d commands\n", k, genCount[k])
	}
	if *verbose {
		for _, r := range roots {
			fmt.Printf("  %-24s children=%d gen=%q\n", r.name, len(r.kids), r.gen)
		}
	}
}

// loadCorpus reads the corpus, transparently handling the gzipped form it is
// committed in.
func loadCorpus(path string) []*tnode {
	f, err := os.Open(path)
	if err != nil {
		fail("open %s: %v", path, err)
	}
	defer f.Close()

	var r io.Reader = f
	var magicBuf [2]byte
	if _, err := io.ReadFull(f, magicBuf[:]); err != nil {
		fail("read %s: %v", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		fail("seek %s: %v", path, err)
	}
	if magicBuf[0] == 0x1f && magicBuf[1] == 0x8b {
		zr, err := gzip.NewReader(f)
		if err != nil {
			fail("gzip %s: %v", path, err)
		}
		defer zr.Close()
		r = zr
	}

	var in []jnode
	dec := json.NewDecoder(r)
	if err := dec.Decode(&in); err != nil {
		fail("decode %s: %v", path, err)
	}
	if len(in) == 0 {
		fail("decode %s: no commands", path)
	}
	roots := make([]*tnode, 0, len(in))
	for i := range in {
		roots = append(roots, in[i].tnode())
	}
	return sortTree(roots)
}

// tnode is one command in nesting order, before flattening.
type tnode struct {
	name, desc, gen string
	opts            []jopt
	kids            []*tnode
}

func (n *jnode) tnode() *tnode {
	out := &tnode{name: n.Name, desc: n.Desc, gen: n.Gen, opts: n.Opts}
	if len(n.Kids) > 0 {
		out.kids = make([]*tnode, 0, len(n.Kids))
		for i := range n.Kids {
			out.kids = append(out.kids, n.Kids[i].tnode())
		}
	}
	return out
}

// sortTree orders every sibling list by name and drops repeats: the loader's
// binary search over children depends on that being a sorted unique key.
func sortTree(in []*tnode) []*tnode {
	sort.Slice(in, func(i, j int) bool { return in[i].name < in[j].name })
	out := in[:1]
	for _, n := range in[1:] {
		if n.name == out[len(out)-1].name {
			continue
		}
		out = append(out, n)
	}
	for _, n := range out {
		if len(n.kids) > 0 {
			n.kids = sortTree(n.kids)
		}
	}
	return out
}

// flatIndex is the whole tree in table form: nodes, one contiguous child-index
// slice, and one contiguous option slice.
type flatIndex struct {
	nodes    []flatNode
	children []uint32
	opts     []flatOpt
	root     uint32
	blob     []byte
}

type flatNode struct {
	name, desc string
	firstChild uint32
	nChild     uint32
	firstOpt   uint32
	nOpt       uint32
	gen        string
}

type flatOpt struct {
	name, desc string
}

func flatten(roots []*tnode) *flatIndex {
	ix := &flatIndex{}
	root := &tnode{name: "", kids: roots}
	ix.root = ix.visit(root)
	return ix
}

func (ix *flatIndex) visit(n *tnode) uint32 {
	idx := uint32(len(ix.nodes))
	ix.nodes = append(ix.nodes, flatNode{name: n.name, desc: n.desc, gen: n.gen})

	firstChild := uint32(len(ix.children))
	ix.children = append(ix.children, make([]uint32, len(n.kids))...)
	for i, k := range n.kids {
		ix.children[int(firstChild)+i] = ix.visit(k)
	}

	firstOpt := uint32(len(ix.opts))
	for _, o := range n.opts {
		ix.opts = append(ix.opts, flatOpt{name: o.Name, desc: o.Desc})
	}

	ix.nodes[idx].firstChild = firstChild
	ix.nodes[idx].nChild = uint32(len(n.kids))
	ix.nodes[idx].firstOpt = firstOpt
	ix.nodes[idx].nOpt = uint32(len(n.opts))
	return idx
}

func encode(ix *flatIndex) ([]byte, error) {
	strs := newStringTable()
	nodeRec := make([][8]uint32, len(ix.nodes))
	for i := range ix.nodes {
		n := &ix.nodes[i]
		genOff := noStr
		if n.gen != "" {
			genOff = strs.add(n.gen)
		}
		nodeRec[i] = [8]uint32{
			strs.add(n.name), strs.add(n.desc),
			n.firstChild, n.nChild,
			n.firstOpt, n.nOpt,
			genOff, 0,
		}
	}
	optRec := make([][2]uint32, len(ix.opts))
	for i := range ix.opts {
		optRec[i] = [2]uint32{strs.add(ix.opts[i].name), strs.add(ix.opts[i].desc)}
	}
	ix.blob = strs.blob

	var buf bytes.Buffer
	buf.WriteString(magic)
	put32(&buf, uint32(len(ix.blob)))
	buf.Write(ix.blob)
	put32(&buf, uint32(len(nodeRec)))
	for _, r := range nodeRec {
		for _, v := range r {
			put32(&buf, v)
		}
	}
	put32(&buf, uint32(len(ix.children)))
	for _, c := range ix.children {
		put32(&buf, c)
	}
	put32(&buf, uint32(len(optRec)))
	for _, r := range optRec {
		put32(&buf, r[0])
		put32(&buf, r[1])
	}
	put32(&buf, ix.root)
	return buf.Bytes(), nil
}

func put32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

// stringTable interns strings once: shared descriptions across commands cost one
// blob entry, and the loader hands out zero-copy views into it.
type stringTable struct {
	blob  []byte
	index map[string]uint32
}

func newStringTable() *stringTable {
	return &stringTable{index: make(map[string]uint32, 1<<14)}
}

func (t *stringTable) add(s string) uint32 {
	if off, ok := t.index[s]; ok {
		return off
	}
	off := uint32(len(t.blob))
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(len(s)))
	t.blob = append(t.blob, b[:]...)
	t.blob = append(t.blob, s...)
	t.index[s] = off
	return off
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genspecs: "+format+"\n", args...)
	os.Exit(1)
}
