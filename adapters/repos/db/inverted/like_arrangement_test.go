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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLikeArrangement(t *testing.T) {
	tests := []struct {
		pattern string
		runs    []string
		gaps    []likeGap
	}{
		{pattern: "abc", runs: []string{"abc"}, gaps: []likeGap{{}, {}}},
		{pattern: "a*c", runs: []string{"a", "c"}, gaps: []likeGap{{}, {hasStar: true}, {}}},
		{pattern: "a?c", runs: []string{"a", "c"}, gaps: []likeGap{{}, {min: 1}, {}}},
		{pattern: "*a*", runs: []string{"a"}, gaps: []likeGap{{hasStar: true}, {hasStar: true}}},
		{pattern: "car?", runs: []string{"car"}, gaps: []likeGap{{}, {min: 1}}},
		{pattern: "a?*c", runs: []string{"a", "c"}, gaps: []likeGap{{}, {hasStar: true, min: 1}, {}}},
		{pattern: "*a*b*", runs: []string{"a", "b"},
			gaps: []likeGap{{hasStar: true}, {hasStar: true}, {hasStar: true}}},
	}

	for _, tc := range tests {
		t.Run(tc.pattern, func(t *testing.T) {
			arr := parseLikeArrangement([]byte(tc.pattern))
			require.Len(t, arr.runs, len(tc.runs))
			for i, run := range tc.runs {
				assert.Equal(t, run, string(arr.runs[i].lit))
			}
			assert.Equal(t, tc.gaps, arr.gaps)
		})
	}
}

func TestLikeArrangementCrossesTokenBoundaries(t *testing.T) {
	// only patterns with at least two literal runs and at least one wildcard
	// can match a doc whose matching tokens are split across keys
	assert.False(t, parseLikeArrangement([]byte("abc")).crossesTokenBoundaries())
	assert.False(t, parseLikeArrangement([]byte("a*")).crossesTokenBoundaries())
	assert.False(t, parseLikeArrangement([]byte("*a")).crossesTokenBoundaries())
	assert.False(t, parseLikeArrangement([]byte("*")).crossesTokenBoundaries())
	assert.False(t, parseLikeArrangement([]byte("??")).crossesTokenBoundaries())
	assert.True(t, parseLikeArrangement([]byte("a*c")).crossesTokenBoundaries())
	assert.True(t, parseLikeArrangement([]byte("a?c")).crossesTokenBoundaries())
	assert.True(t, parseLikeArrangement([]byte("wild*chars")).crossesTokenBoundaries())
}

func TestLikeArrangementMatchTokens(t *testing.T) {
	tests := []struct {
		pattern string
		keys    [][]byte
		want    bool
	}{
		// issue #13565 gate: "a.c" is indexed as the tokens ["a", "c"], and
		// the strictly more permissive wildcards must reach it
		{pattern: "a*c", keys: [][]byte{[]byte("a"), []byte("c")}, want: true},
		{pattern: "a?c", keys: [][]byte{[]byte("a"), []byte("c")}, want: true},
		{pattern: "a*c", keys: [][]byte{[]byte("abc")}, want: true},
		{pattern: "a?c", keys: [][]byte{[]byte("abc")}, want: true},
		{pattern: "a*c", keys: [][]byte{[]byte("aXc")}, want: true},
		{pattern: "a?c", keys: [][]byte{[]byte("aXc")}, want: true},
		// "a.c" arrives at this layer as one literal run (the word-tokenized
		// pipeline splits it into the two terms "a" and "c" first, and ANDs
		// them; see TestLikeWildcardsMatchTokensSplitAtPunctuation)
		{pattern: "a.c", keys: [][]byte{[]byte("a.c")}, want: true},
		{pattern: "a.c", keys: [][]byte{[]byte("a"), []byte("c")}, want: false},
		{pattern: "a.c", keys: [][]byte{[]byte("abc")}, want: false},
		{pattern: "a.c", keys: [][]byte{[]byte("aXc")}, want: false},

		// one-token arrangements match exactly like the per-key regexp
		{pattern: "a*c", keys: [][]byte{[]byte("ac")}, want: true},
		{pattern: "a*c", keys: [][]byte{[]byte("a")}, want: false},
		{pattern: "a*c", keys: [][]byte{[]byte("c")}, want: false},
		{pattern: "a?c", keys: [][]byte{[]byte("ac")}, want: false},
		{pattern: "a?c", keys: [][]byte{[]byte("ab"), []byte("c")}, want: false},
		{pattern: "a?c", keys: [][]byte{[]byte("a"), []byte("bc")}, want: false},

		// '?' matches exactly one character, the token boundary included
		{pattern: "a?c", keys: [][]byte{[]byte("a"), []byte("x"), []byte("c")}, want: true},
		{pattern: "a??c", keys: [][]byte{[]byte("a"), []byte("c")}, want: false},
		{pattern: "a??c", keys: [][]byte{[]byte("ab"), []byte("c")}, want: true},

		// '*' spans any number of characters, token boundaries included;
		// tokens that carry no literal run are dropped from the arrangement
		{pattern: "wild*chars", keys: [][]byte{[]byte("wild"), []byte("card"), []byte("chars")}, want: true},
		{pattern: "wild*card*chars",
			keys: [][]byte{[]byte("wild"), []byte("card"), []byte("chars")}, want: true},
		{pattern: "wild*chars", keys: [][]byte{[]byte("wild"), []byte("junk"), []byte("chars")}, want: true},
		{pattern: "wild*chars", keys: [][]byte{[]byte("wild"), []byte("card")}, want: false},

		// a token may carry several runs, in pattern order
		{pattern: "a*b*c", keys: [][]byte{[]byte("abc")}, want: true},
		{pattern: "a*b*c", keys: [][]byte{[]byte("ab"), []byte("c")}, want: true},
		{pattern: "b*a", keys: [][]byte{[]byte("ab")}, want: false},

		// runs are never reused across two positions: "ab*ba" needs two
		// tokens or the single key "abba", but not the key "aba"
		{pattern: "ab*ba", keys: [][]byte{[]byte("aba")}, want: false},
		{pattern: "ab*ba", keys: [][]byte{[]byte("abba")}, want: true},
		{pattern: "ab*ba", keys: [][]byte{[]byte("ab"), []byte("ba")}, want: true},

		// the historical single-token meaning is fully preserved
		{pattern: "car?", keys: [][]byte{[]byte("cart")}, want: true},
		{pattern: "car?", keys: [][]byte{[]byte("car")}, want: false},
		{pattern: "*c?r*", keys: [][]byte{[]byte("car")}, want: true},
		{pattern: "*c?r*", keys: [][]byte{[]byte("c"), []byte("r")}, want: true},
		{pattern: "*c?r*", keys: [][]byte{[]byte("cr")}, want: false},
		{pattern: "modulo-1*", keys: [][]byte{[]byte("modulo-12")}, want: true},
		{pattern: "modulo-1*", keys: [][]byte{[]byte("xmodulo-12")}, want: false},

		// no keys, no match
		{pattern: "a*c", keys: nil, want: false},
	}

	for _, tc := range tests {
		name := tc.pattern
		for _, k := range tc.keys {
			name += "/" + string(k)
		}
		t.Run(name, func(t *testing.T) {
			arr := parseLikeArrangement([]byte(tc.pattern))
			assert.Equal(t, tc.want, arr.matchTokens(tc.keys))
		})
	}
}
