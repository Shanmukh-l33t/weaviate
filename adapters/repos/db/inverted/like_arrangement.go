//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
// \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//  \_/\_/ \___|\__,_| \_/ |_| \__,_|\__\___|
//
//  Copyright © 2016 - 2026 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io

package inverted

import (
	"bytes"
	"context"
	"fmt"

	"github.com/weaviate/sroar"
	"github.com/weaviate/weaviate/adapters/repos/db/lsmkv"
	"github.com/weaviate/weaviate/entities/filters"
)

// This file implements Like wildcard matching over the token set of a single
// document (issue #13565). On a tokenized text property the stored keys are
// the document's tokens: the word tokenizer splits values at punctuation, so
// the value "a.c" is indexed under the two keys "a" and "c". A per-key
// wildcard match can therefore never reach it: the single term "a*c" is
// matched against one key at a time and no key is "a.c". Yet "a.c" (which the
// pattern language decomposes into the two exact terms "a" and "c") does
// match, so the more permissive pattern returned a strict subset of the
// narrower one, silently losing documents.
//
// The fix matches a wildcard pattern against the document's whole token set:
// a pattern matches when some of the document's tokens can be arranged in some
// order and joined with a single separator character to a string matching the
// glob, where '*' matches any sequence of characters (the separator included)
// and '?' matches exactly one character (the separator included). The
// arrangement is unordered because the inverted index does not retain token
// order; the literal patterns' existing AND-of-token-globs meaning is
// preserved exactly (it is the single-token case of the same model).
//
// Remaining known limitation: a '?'-chain that spans more than one token
// boundary while intermediate tokens are fully consumed as gap filler (e.g.
// pattern "a???c" over the tokens ["a", "x", "c"]) is not matched; such gaps
// are only modeled within one token or across exactly one boundary. '*'-gaps
// are not affected: they can drop or absorb intermediate tokens freely.

// likeArrangement is a parsed Like wildcard pattern, ready to be matched
// against the token set of one document.
type likeArrangement struct {
	// runs holds the literal runs of the pattern in order; every run matches
	// inside a single token. runs is never empty for a parsed pattern.
	runs []likeRun
	// gaps holds len(runs)+1 wildcard gaps: the leading gap, one gap between
	// each pair of consecutive runs and the trailing gap.
	gaps []likeGap
}

type likeRun struct{ lit []byte }

// likeGap is a maximal sequence of '?' and '*' wildcards in the pattern. The
// gap covers exactly min characters when it holds no '*', and at least min
// characters when it does.
type likeGap struct {
	hasStar bool
	min     int // number of '?' wildcards in the gap
}

func (g likeGap) fits(dist int) bool {
	if dist < g.min {
		return false
	}
	return g.hasStar || dist == g.min
}

func (g likeGap) empty() bool { return !g.hasStar && g.min == 0 }

// parseLikeArrangement splits a Like pattern into literal runs and wildcard
// gaps. Literal bytes are kept verbatim (no regexp quoting here): runs are
// compared against token bytes directly.
func parseLikeArrangement(in []byte) *likeArrangement {
	arr := &likeArrangement{}
	i := 0
	readGap := func() likeGap {
		var g likeGap
		for i < len(in) && (in[i] == '?' || in[i] == '*') {
			if in[i] == '*' {
				g.hasStar = true
			} else {
				g.min++
			}
			i++
		}
		return g
	}

	arr.gaps = append(arr.gaps, readGap())
	for i < len(in) {
		start := i
		for i < len(in) && in[i] != '?' && in[i] != '*' {
			i++
		}
		arr.runs = append(arr.runs, likeRun{lit: in[start:i]})
		arr.gaps = append(arr.gaps, readGap())
	}
	return arr
}

// crossesTokenBoundaries reports whether this pattern can match a document
// whose matching tokens are split across several keys. Only patterns with at
// least two literal runs can: a single run always lies within one token, and
// the edge gaps of this model never cross a token boundary. Patterns without
// this capability keep the historical per-key match unchanged.
func (la *likeArrangement) crossesTokenBoundaries() bool {
	if len(la.runs) < 2 {
		return false
	}
	for _, g := range la.gaps {
		if !g.empty() {
			return true
		}
	}
	// no wildcards at all: the pattern is one literal run once merged, but it
	// arrived split, which parseLikeArrangement does not do; nothing to gain.
	return false
}

// likeArrangementMaxTokens bounds the arrangement search. Documents with more
// run-bearing keys than this fall back to a containment approximation that
// never misses a match (it may over-match, preserving the subset relation).
const likeArrangementMaxTokens = 64

// matchTokens reports whether the pattern matches the given token set. The
// keys must be distinct (they come from one document's index entries).
func (la *likeArrangement) matchTokens(keys [][]byte) bool {
	if len(la.runs) == 0 || len(keys) == 0 {
		return false
	}
	if len(keys) > likeArrangementMaxTokens {
		return la.matchTokensApprox(keys)
	}

	m := &arrangementMatcher{la: la, keys: keys}
	// leading gap: run 0 starts at offset o of its token, which is the first
	// token of the arrangement, so the gap covers exactly the o bytes before it
	for t, key := range keys {
		for _, o := range runOccurrences(key, la.runs[0].lit) {
			if la.gaps[0].fits(o) && m.place(1, t, o+len(la.runs[0].lit), 1<<t) {
				return true
			}
		}
	}
	return false
}

type arrangementMatcher struct {
	la   *likeArrangement
	keys [][]byte
	// memo deduplicates failed states; the used-set is part of the key.
	memo map[arrangementState]bool
}

type arrangementState struct {
	run, tok, end int
	used          uint64
}

// place positions runs[run] and, recursively, all later runs. prevTok/prevEnd
// locate the end of runs[run-1] within its token; used is the set of tokens
// already placed. Tokens are placed at most once each.
func (m *arrangementMatcher) place(run, prevTok, prevEnd int, used uint64) bool {
	if run == len(m.la.runs) {
		// trailing gap covers the rest of the last token; further tokens can
		// simply be dropped from the arrangement (a subset is always allowed)
		return m.la.gaps[run].fits(len(m.keys[prevTok]) - prevEnd)
	}

	state := arrangementState{run: run, tok: prevTok, end: prevEnd, used: used}
	if m.memo == nil {
		m.memo = map[arrangementState]bool{}
	}
	if done, ok := m.memo[state]; ok {
		return done
	}
	m.memo[state] = false // pessimistic entry breaks cycles (there are none, but be safe)

	gap := m.la.gaps[run]
	runLit := m.la.runs[run].lit
	for t, key := range m.keys {
		sameToken := t == prevTok
		if !sameToken && used&(1<<t) != 0 {
			continue
		}
		for _, o := range runOccurrences(key, runLit) {
			var dist int
			switch {
			case sameToken:
				if o < prevEnd {
					continue
				}
				dist = o - prevEnd
			default:
				// the run sits in another token: the gap covers the tail of
				// the previous token, one separator character and the head of
				// this token up to the run. Intermediate tokens are only
				// reachable through '*' gaps, which drop them just as well.
				dist = (len(m.keys[prevTok]) - prevEnd) + 1 + o
			}
			if !gap.fits(dist) {
				continue
			}
			if m.place(run+1, t, o+len(runLit), used|1<<t) {
				m.memo[state] = true
				return true
			}
		}
	}
	return false
}

// matchTokensApprox is the fallback for documents with an impractical number
// of run-bearing keys: every literal run must occur in some key. This can not
// miss an arrangement match (every run lies within one token of any matching
// arrangement), so the subset relation is preserved; it may over-match.
func (la *likeArrangement) matchTokensApprox(keys [][]byte) bool {
	for _, run := range la.runs {
		found := false
		for _, key := range keys {
			if len(run.lit) > 0 && bytes.Contains(key, run.lit) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// runOccurrences returns every offset at which run occurs in key. Occurrences
// may overlap.
func runOccurrences(key, run []byte) []int {
	if len(run) == 0 {
		return nil
	}
	var out []int
	for i := 0; i+len(run) <= len(key); {
		j := bytes.Index(key[i:], run)
		if j < 0 {
			break
		}
		out = append(out, i+j)
		i += j + 1
	}
	return out
}

// docBitmapLikeArrangement resolves one Like term with token-arrangement
// semantics. It gathers every key of the property's bucket that contains one
// of the pattern's literal runs (any matching arrangement can only use such
// keys), groups them by doc id and matches each document's token set. Keys
// that contain no run can never participate and are skipped, which also keeps
// the gather small for typical patterns.
func (s *Searcher) docBitmapLikeArrangement(ctx context.Context, b *lsmkv.Bucket,
	pv *propValuePair,
) (docBitmap, error) {
	la := pv.likeArr
	docKeys := map[uint64]map[string]struct{}{}

	readFn := func(k []byte, docIDs *sroar.Bitmap, release func()) (bool, error) {
		for _, id := range docIDs.ToArray() {
			keys := docKeys[id]
			if keys == nil {
				keys = map[string]struct{}{}
				docKeys[id] = keys
			}
			keys[string(k)] = struct{}{}
		}
		release()
		return true, nil
	}

	for _, run := range la.runs {
		if len(run.lit) == 0 {
			continue
		}
		// "*<run>*" matches every key that contains the run anywhere
		candidate := make([]byte, 0, len(run.lit)+2)
		candidate = append(candidate, '*')
		candidate = append(candidate, run.lit...)
		candidate = append(candidate, '*')

		switch b.Strategy() {
		case lsmkv.StrategySetCollection:
			rr := NewRowReader(b, candidate, filters.OperatorLike, false)
			if err := rr.Read(ctx, readFn); err != nil {
				return newDocBitmap(), fmt.Errorf("read like arrangement candidates: %w", err)
			}
		case lsmkv.StrategyRoaringSet:
			rr := NewRowReaderRoaringSet(b, candidate, filters.OperatorLike, false)
			if err := rr.Read(ctx, readFn); err != nil {
				return newDocBitmap(), fmt.Errorf("read like arrangement candidates: %w", err)
			}
		case lsmkv.StrategyMapCollection, lsmkv.StrategyInverted:
			rr := NewRowReaderFrequency(b, candidate, filters.OperatorLike, false, s.shardVersion)
			if err := rr.Read(ctx, readFn); err != nil {
				return newDocBitmap(), fmt.Errorf("read like arrangement candidates: %w", err)
			}
		default:
			return newDocBitmap(), fmt.Errorf(
				"like token arrangement unsupported for bucket strategy %q on prop %q",
				b.Strategy(), pv.prop)
		}
	}

	out := newDocBitmap()
	for id, keySet := range docKeys {
		keys := make([][]byte, 0, len(keySet))
		for k := range keySet {
			keys = append(keys, []byte(k))
		}
		if la.matchTokens(keys) {
			out.docIDs.Set(id)
		}
	}
	return out, nil
}
