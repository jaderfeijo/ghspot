package main

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sahilm/fuzzy"
)

type result struct {
	it      *Item
	matched map[int]bool // byte offsets into it.Title to highlight
	repoHit bool         // matched through the leading repo word
}

var numRe = regexp.MustCompile(`^#?(\d+)$`)

func numberQuery(q string) (int, bool) {
	m := numRe.FindStringSubmatch(strings.TrimSpace(q))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil && n > 0
}

// parseQuery splits a leading repo word off q: "glamour 42" scopes to every
// known repo whose name (or owner/name) starts with "glamour" and leaves "42".
// It only applies when tickets span several repos, and returns no repos when
// the first word doesn't match one, so plain title searches pass through.
func parseQuery(q string, repos []string) (matched []string, rest string) {
	q = strings.TrimSpace(q)
	if len(repos) < 2 {
		return nil, q
	}
	first, rest, _ := strings.Cut(q, " ")
	w := strings.ToLower(strings.TrimSuffix(first, "/"))
	if w == "" || strings.HasPrefix(w, "#") {
		return nil, q
	}
	for _, r := range repos {
		lr := strings.ToLower(r)
		_, name, _ := strings.Cut(lr, "/")
		if strings.HasPrefix(name, w) || strings.HasPrefix(lr, w) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return nil, q
	}
	return matched, strings.TrimSpace(rest)
}

// filterItems ranks items against q. With a leading repo word (see
// parseQuery), only tickets in the matching repos are considered.
func filterItems(items []*Item, q string, repos []string, keep func(*Item) bool) []result {
	scoped, rest := parseQuery(q, repos)
	if scoped == nil {
		return rank(items, rest, keep)
	}
	in := map[string]bool{}
	for _, r := range scoped {
		in[r] = true
	}
	var sub []*Item
	for _, it := range items {
		if in[it.Repo] {
			sub = append(sub, it)
		}
	}
	out := rank(sub, rest, keep)
	for i := range out {
		out[i].repoHit = true
	}
	return out
}

// rank orders items against q. Items are expected newest-first, which is
// preserved as the tiebreak. A numeric query ("123" or "#123") puts the
// exact number first, even if the type/state filters would hide it, then
// numbers starting with those digits, then fuzzy title matches.
func rank(items []*Item, q string, keep func(*Item) bool) []result {
	q = strings.TrimSpace(q)
	var out []result
	if q == "" {
		for _, it := range items {
			if keep(it) {
				out = append(out, result{it: it})
			}
		}
		return out
	}

	seen := map[*Item]bool{}
	if n, ok := numberQuery(q); ok {
		digits := strconv.Itoa(n)
		var prefix []result
		for _, it := range items {
			s := strconv.Itoa(it.Number)
			switch {
			case it.Number == n:
				out = append(out, result{it: it})
				seen[it] = true
			case strings.HasPrefix(s, digits) && keep(it):
				prefix = append(prefix, result{it: it})
				seen[it] = true
			}
		}
		out = append(out, prefix...)
	}

	var cands []*Item
	var titles []string
	for _, it := range items {
		if !seen[it] && keep(it) {
			cands = append(cands, it)
			titles = append(titles, it.Title)
		}
	}

	// Every word must fuzzy-match the title, in any order.
	words := strings.Fields(strings.TrimPrefix(q, "#"))
	type acc struct {
		score   int
		hits    int
		matched map[int]bool
	}
	accs := make([]acc, len(cands))
	for _, w := range words {
		lw := strings.ToLower(w)
		for _, m := range fuzzy.Find(w, titles) {
			idx := m.MatchedIndexes
			score := m.Score
			// Literal hits beat fuzzy ones, word-start hits most of all, and
			// highlight the literal text rather than the fuzzy letters.
			lt := strings.ToLower(titles[m.Index])
			if i := strings.Index(lt, lw); i >= 0 && len(lt) == len(titles[m.Index]) {
				score += 100
				if i == 0 || !isWordRune(lt[i-1]) {
					score += 50
				}
				idx = idx[:0:0]
				for j := i; j < i+len(lw); j++ {
					idx = append(idx, j)
				}
			} else if idx[len(idx)-1]-idx[0]+1 > 2*len(w)+2 {
				// Letters scattered across the title aren't a match; typos
				// and skipped letters stay close together.
				continue
			}
			a := &accs[m.Index]
			a.score += score
			a.hits++
			if a.matched == nil {
				a.matched = map[int]bool{}
			}
			for _, i := range idx {
				a.matched[i] = true
			}
		}
	}
	idx := make([]int, 0, len(cands))
	for i, a := range accs {
		if a.hits == len(words) {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return accs[idx[a]].score > accs[idx[b]].score })
	for _, i := range idx {
		out = append(out, result{it: cands[i], matched: accs[i].matched})
	}
	return out
}

func isWordRune(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}
