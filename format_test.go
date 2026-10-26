package sev

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"
)

// buildBlob assembles a specdb blob the way tools/genspecs does, so the loader
// has something well-formed to accept — and something to truncate. It returns
// the string offsets too, so a test can name the table entries it means.
func buildBlob(strs []string, nodes [][8]uint32, children []uint32, opts [][2]uint32, root uint32) ([]byte, []uint32) {
	offsets := make([]uint32, len(strs))
	var blob []byte
	for i, s := range strs {
		offsets[i] = uint32(len(blob))
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(s)))
		blob = append(blob, b[:]...)
		blob = append(blob, s...)
	}
	out := []byte(specDBMagic)
	put := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		out = append(out, b[:]...)
	}
	put(uint32(len(blob)))
	out = append(out, blob...)
	put(uint32(len(nodes)))
	for _, n := range nodes {
		for _, v := range n {
			put(v)
		}
	}
	put(uint32(len(children)))
	for _, c := range children {
		put(c)
	}
	put(uint32(len(opts)))
	for _, o := range opts {
		put(o[0])
		put(o[1])
	}
	put(root)
	return out, offsets
}

// tinyBlob builds the tree "" -> git -> {checkout, commit}, where git also has
// one flag.
func tinyBlob() []byte {
	strs := []string{"", "git", "version control", "checkout", "switch branches",
		"commit", "record changes", "--amend", "rewrite"}
	children := []uint32{1, 2, 3}
	opts := [][2]uint32{{0, 0}}
	nodes := [][8]uint32{
		{0, 0, 0, 1, 0, 0, noStr, 0}, // root -> [git]
		{0, 0, 1, 2, 0, 1, noStr, 0}, // git -> [checkout, commit] + 1 flag
		{0, 0, 0, 0, 0, 0, noStr, 0},
		{0, 0, 0, 0, 0, 0, noStr, 0},
	}
	// Offsets come from the builder, so the two passes share one layout.
	_, off := buildBlob(strs, nodes, children, opts, 0)
	nodes[1][0], nodes[1][1] = off[1], off[2]
	nodes[2][0], nodes[2][1] = off[3], off[4]
	nodes[3][0], nodes[3][1] = off[5], off[6]
	opts[0][0], opts[0][1] = off[7], off[8]
	blob, _ := buildBlob(strs, nodes, children, opts, 0)
	return blob
}

func TestLoadIndex(t *testing.T) {
	ix, err := loadIndex(tinyBlob())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := ix.name(ix.root); got != "" {
		t.Fatalf("root name = %q", got)
	}
	git, ok := ix.child(ix.root, "git")
	if !ok {
		t.Fatal("git not found under the root")
	}
	if got := ix.desc(git); got != "version control" {
		t.Fatalf("desc = %q", got)
	}
	if got := ix.str(ix.optSlice(git)[0].name); got != "--amend" {
		t.Fatalf("option = %q", got)
	}

	kids := ix.childRange(git, "c")
	if len(kids) != 2 {
		t.Fatalf("childRange(c) = %d, want checkout and commit", len(kids))
	}
	if got := ix.childRange(git, "commit"); len(got) != 1 {
		t.Fatalf("childRange(commit) = %d, want 1", len(got))
	}
	if got := ix.childRange(git, "zz"); got != nil {
		t.Fatalf("childRange(zz) = %v, want nil", got)
	}
	if got := ix.childRange(git, "c"); ix.name(got[0]) != "checkout" || ix.name(got[1]) != "commit" {
		t.Fatalf("children are not in bytewise name order: %q", ix.name(got[0]))
	}
}

func TestLoadIndexRejectsGarbage(t *testing.T) {
	if _, err := loadIndex([]byte("not a specdb")); !errors.Is(err, ErrFormat) {
		t.Fatalf("err = %v, want ErrFormat", err)
	}
}

// TestLoadIndexTruncated feeds every prefix of a valid blob to the loader: a
// short read must be an error, never a panic and never a half-built index that
// panics later on a keystroke.
func TestLoadIndexTruncated(t *testing.T) {
	blob := tinyBlob()
	for n := 0; n < len(blob); n++ {
		if _, err := loadIndex(blob[:n]); err == nil {
			t.Fatalf("truncation at %d/%d was accepted", n, len(blob))
		}
	}
}

func TestLoadIndexCorrupt(t *testing.T) {
	base := tinyBlob()
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		mutated := append([]byte(nil), base...)
		for flips := 0; flips < 3; flips++ {
			mutated[len(specDBMagic)+rng.Intn(len(mutated)-len(specDBMagic))] = byte(rng.Intn(256))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on mutated blob %d: %v", i, r)
				}
			}()
			if ix, err := loadIndex(mutated); err == nil && ix != nil {
				// A mutation inside a string body is legal; the lookups must
				// still stay inside the blob.
				_ = ix.name(ix.root)
				for _, kid := range ix.childSlice(ix.root) {
					_ = ix.name(kid)
					_ = ix.desc(kid)
				}
			}
		}()
	}
}

func TestPrefixUpperBound(t *testing.T) {
	for in, want := range map[string]string{"co": "cp", "a": "b"} {
		got, ok := prefixUpperBound(in)
		if !ok || got != want {
			t.Fatalf("%q -> (%q, %v), want %q", in, got, ok, want)
		}
	}
	if got, ok := prefixUpperBound("zz"); !ok || got != "z{" {
		t.Fatalf("zz -> (%q, %v), want z{", got, ok)
	}
	if _, ok := prefixUpperBound("\xff\xff"); ok {
		t.Fatal("an all-0xFF prefix has no upper bound")
	}
}
