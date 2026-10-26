package sev

import _ "embed"

// specDB is the compiled command tree, produced by tools/genspecs from a
// command-spec source and embedded so the engine needs no file to start.
//
//go:embed specdb.bin.gz
var specDB []byte
