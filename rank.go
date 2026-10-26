package sev

// Score weights. Match quality dominates — a prefix hit must never lose to a
// fuzzy hit just because the fuzzy one has history — while frecency and
// transitions reorder commands of equal footing, which is the common case
// ("git ch" against five checkout-ish commands).
const (
	weightMatch      = 1.0
	weightFrecency   = 0.45
	weightTransition = 0.30
)

// matchTier maps how well name answers partial onto 0..1.
func matchTier(partial, name string) float64 {
	switch {
	case partial == "":
		return 0.85 // nothing typed: history and order decide, not match
	case name == partial:
		return 1.0
	case hasPrefix(name, partial):
		return 0.85
	case wordPrefix(name, partial):
		return 0.65
	default:
		return 0.5
	}
}

// wordPrefix matches a prefix of a word inside name, so "check" finds
// "git-checkout" and "docker-compose" answers to "comp".
func wordPrefix(name, partial string) bool {
	for i := 0; i+len(partial) <= len(name); i++ {
		if i > 0 && !isWordBreak(name[i-1]) {
			continue
		}
		if hasPrefix(name[i:], partial) {
			return true
		}
	}
	return false
}

func isWordBreak(c byte) bool {
	return c == '-' || c == '_' || c == '.' || c == '/' || c == ':'
}

// subsequenceScore is the typo-tolerant fallback: every byte of partial must
// appear in order in name. Consecutive hits and word starts score higher, so
// "gco" prefers "git checkout" over "gcc".
func subsequenceScore(partial, name string) (float64, bool) {
	if partial == "" || len(partial) > len(name) {
		return 0, false
	}
	pi, hits, streak, best := 0, 0, 0, 0
	firstAt := -1
	for ni := 0; ni < len(name) && pi < len(partial); ni++ {
		if lowerASCII(name[ni]) != lowerASCII(partial[pi]) {
			streak = 0
			continue
		}
		if firstAt < 0 {
			firstAt = ni
		}
		hits++
		pi++
		streak++
		if streak > best {
			best = streak
		}
	}
	if pi < len(partial) {
		return 0, false
	}
	// Density: how much of the name the match covers, plus a bonus for
	// contiguity and for starting at a word boundary.
	density := float64(hits) / float64(len(name))
	score := 0.20 + 0.30*density + 0.20*float64(best)/float64(len(partial))
	if firstAt == 0 || isWordBreak(name[firstAt-1]) {
		score += 0.15
	}
	if score > 0.5 {
		score = 0.5 // fuzzy never outranks a prefix match
	}
	return score, true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// appendUnique adds c unless the same command is already present. Two rows are
// the same command when they differ only by the trailing space that marks "more
// can follow". The already-present row keeps its identity (a spec row carries a
// description; a history row does not), but adopts the more specific variant.
func appendUnique(out []candidate, c candidate) []candidate {
	if c.text == "" && c.name == "" {
		return out
	}
	for i := range out {
		if !out[i].sameKey(&c) {
			continue
		}
		// Same command from two sources: keep one row but let the more specific
		// facts win — a spec child that takes arguments still allows the space,
		// and a history line is a better completion text than a bare name.
		if c.space && !out[i].space {
			out[i].space = true
		}
		if out[i].text == "" && c.text != "" && !out[i].space {
			out[i].text = c.text
		}
		if out[i].desc == "" {
			out[i].desc = c.desc
		}
		if c.match > out[i].match {
			out[i].match = c.match
		}
		return out
	}
	return append(out, c)
}
