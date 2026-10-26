package sev

import "testing"

func TestScan(t *testing.T) {
	tests := []struct {
		line    string
		tokens  []string
		partial bool
		seg     int
	}{
		{"", nil, false, 0},
		{"git", []string{"git"}, true, 0},
		{"git ", []string{"git"}, false, 0},
		{"git ch", []string{"git", "ch"}, true, 0},
		{"  git   ch", []string{"git", "ch"}, true, 0},
		{"cd /x && git ch", []string{"git", "ch"}, true, 9},
		{"FOO=bar git st", []string{"git", "st"}, true, 0},
		{"git commit | wc -l", []string{"wc", "-l"}, true, 13},
		{`cat "my file" `, []string{"cat", "my file"}, false, 0},
		{`echo 'a|b'`, []string{"echo", "a|b"}, true, 0},
		{"docker run -it", []string{"docker", "run", "-it"}, true, 0},
		{"ls; ", nil, false, 4},
	}
	for _, tc := range tests {
		toks, seg, partial := scan(tc.line, nil)
		if partial != tc.partial {
			t.Errorf("%q: partial = %v, want %v", tc.line, partial, tc.partial)
		}
		if seg != tc.seg {
			t.Errorf("%q: segStart = %d, want %d", tc.line, seg, tc.seg)
		}
		if len(toks) != len(tc.tokens) {
			t.Errorf("%q: tokens = %q, want %q", tc.line, texts(toks), tc.tokens)
			continue
		}
		for i, tok := range toks {
			if tok.Text != tc.tokens[i] {
				t.Errorf("%q: token %d = %q, want %q", tc.line, i, tok.Text, tc.tokens[i])
			}
			if tok.Start < 0 || tok.Start >= len(tc.line)+1 {
				t.Errorf("%q: token %d start %d out of range", tc.line, i, tok.Start)
			}
		}
	}
}

func texts(toks []Token) []string {
	out := make([]string, len(toks))
	for i, t := range toks {
		out[i] = t.Text
	}
	return out
}

// TestScanAliasesLine checks the property the engine depends on: Start is an
// offset into the original line, quotes included.
func TestScanAliasesLine(t *testing.T) {
	line := `cd /tmp && cat "a b"`
	toks, _, _ := scan(line, nil)
	last := toks[len(toks)-1]
	if last.Text != "a b" {
		t.Fatalf("token text = %q, want unquoted %q", last.Text, "a b")
	}
	if line[last.Start] != '"' {
		t.Fatalf("start %d does not point at the opening quote in %q", last.Start, line)
	}
}

func TestAssignmentPrefix(t *testing.T) {
	toks, _, _ := scan("A=1 B=2 ls", nil)
	if len(toks) != 1 || toks[0].Text != "ls" {
		t.Fatalf("tokens = %q, want [ls]", texts(toks))
	}
}

func BenchmarkScan(b *testing.B) {
	line := "git checkout -b feature/x --orphan"
	b.ReportAllocs()
	for b.Loop() {
		scan(line, nil)
	}
}
