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
	"context"
	"encoding/binary"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/weaviate/weaviate/adapters/repos/db/helpers"
	"github.com/weaviate/weaviate/adapters/repos/db/inverted/stopwords"
	"github.com/weaviate/weaviate/adapters/repos/db/lsmkv"
	"github.com/weaviate/weaviate/adapters/repos/db/roaringset"
	"github.com/weaviate/weaviate/entities/cyclemanager"
	"github.com/weaviate/weaviate/entities/filters"
	"github.com/weaviate/weaviate/entities/models"
	"github.com/weaviate/weaviate/entities/schema"
)

// likeDocs runs the real text-Like pipeline for one pattern against the
// fixture property and returns the matched doc ids.
func likeDocs(t *testing.T, f *containsBatchGateFixture, propName string, pattern string) []uint64 {
	t.Helper()
	return likeDocsWith(t, f.searcher, f.class, propName, pattern)
}

func likeDocsWith(t *testing.T, s *Searcher, class *models.Class, propName string, pattern string) []uint64 {
	t.Helper()
	var prop *models.Property
	for _, p := range class.Properties {
		if p.Name == propName {
			prop = p
		}
	}
	require.NotNil(t, prop, "property %q not in fixture schema", propName)

	pv, err := s.extractTokenizableProp(prop, schema.DataTypeText, pattern,
		filters.OperatorLike, class)
	require.NoError(t, err)
	require.NotNil(t, pv)

	dbm, err := pv.resolveDocIDs(context.Background(), s, 0)
	require.NoError(t, err)
	require.NotNil(t, dbm)
	return dbm.docIDs.ToArray()
}

// TestLikeWildcardsMatchTokensSplitAtPunctuation is the regression gate for
// issue #13565. On a word-tokenized text property the stored tokens of the
// value "a.c" are ["a", "c"]: the tokenizer splits at punctuation. A Like
// wildcard pattern must still reach that document, because "a*c" and "a?c"
// are strictly more permissive than "a.c" in the pattern language (the
// subset relation). Before the fix, "a*c"/"a?c" silently lost it.
func TestLikeWildcardsMatchTokensSplitAtPunctuation(t *testing.T) {
	f := newContainsBatchGateFixture(t)

	// doc 1 = "a.c"     -> word tokens ["a", "c"]
	// doc 2 = "abc"     -> word tokens ["abc"]
	// doc 3 = "aXc"     -> word tokens ["axc"]
	// doc 4 = "wild-card.chars" -> word tokens ["wild", "card", "chars"]
	writeContainsRows(t, f, "prop-text-word", map[string][]uint64{
		"a":     {1},
		"c":     {1},
		"abc":   {2},
		"axc":   {3},
		"wild":  {4},
		"card":  {4},
		"chars": {4},
	})

	t.Run("a.c matches exactly the doc whose tokens are a and c", func(t *testing.T) {
		require.Equal(t, []uint64{1}, likeDocs(t, f, "prop-text-word", "a.c"))
	})

	t.Run("a*c matches every doc a.c matches plus the plain tokens", func(t *testing.T) {
		require.Equal(t, []uint64{1, 2, 3}, likeDocs(t, f, "prop-text-word", "a*c"))
	})

	t.Run("a?c matches every doc a.c matches plus the plain tokens", func(t *testing.T) {
		require.Equal(t, []uint64{1, 2, 3}, likeDocs(t, f, "prop-text-word", "a?c"))
	})

	t.Run("subset relation: L(a.c) is contained in L(a*c) and L(a?c)", func(t *testing.T) {
		dot := likeDocs(t, f, "prop-text-word", "a.c")
		for _, pattern := range []string{"a*c", "a?c"} {
			got := likeDocs(t, f, "prop-text-word", pattern)
			for _, doc := range dot {
				require.Contains(t, got, doc,
					"Like %q must match doc %d, which Like \"a.c\" matches", pattern, doc)
			}
		}
	})

	t.Run("wildcards span punctuation between tokens", func(t *testing.T) {
		require.Equal(t, []uint64{4}, likeDocs(t, f, "prop-text-word", "wild*chars"))
		require.Equal(t, []uint64{4}, likeDocs(t, f, "prop-text-word", "wild*card*chars"))
	})

	t.Run("literal patterns keep their meaning", func(t *testing.T) {
		require.Equal(t, []uint64{1}, likeDocs(t, f, "prop-text-word", "a.c"))
		require.Equal(t, []uint64{2}, likeDocs(t, f, "prop-text-word", "abc"))
		require.Equal(t, []uint64{}, likeDocs(t, f, "prop-text-word", "ac"))
		require.Equal(t, []uint64{4}, likeDocs(t, f, "prop-text-word", "wild"))
		// a pattern split into literal segments keeps its AND-of-token-globs
		// meaning: every segment must match some token of the doc
		require.Equal(t, []uint64{4}, likeDocs(t, f, "prop-text-word", "wild chars"))
		require.Equal(t, []uint64{}, likeDocs(t, f, "prop-text-word", "wild missing"))
	})
}

// TestLikeWildcardsPunctuatedTokensControl pins the behavior that must not
// change: when the tokenizer keeps punctuation inside a single token
// (whitespace tokenization), the per-token wildcard match already works and
// must keep working exactly as before.
func TestLikeWildcardsPunctuatedTokensControl(t *testing.T) {
	f := newContainsBatchGateFixture(t)

	// whitespace tokens keep punctuation: "a.c" is one token
	writeContainsRows(t, f, "prop-text-whitespace", map[string][]uint64{
		"a.c": {5},
		"abc": {6},
		"aXc": {7},
	})

	require.Equal(t, []uint64{5}, likeDocs(t, f, "prop-text-whitespace", "a.c"))
	require.Equal(t, []uint64{5, 6, 7}, likeDocs(t, f, "prop-text-whitespace", "a*c"))
	require.Equal(t, []uint64{5, 6, 7}, likeDocs(t, f, "prop-text-whitespace", "a?c"))
}

// TestLikeWildcardsSearchableBucket repeats the gate on a searchable
// (MapCollection) bucket, the other strategy a text filter can read from.
func TestLikeWildcardsSearchableBucket(t *testing.T) {
	dir := t.TempDir()
	logger := logrus.New()
	store, err := lsmkv.New(dir, dir, logger, nil, nil,
		cyclemanager.NewCallbackGroupNoop(),
		cyclemanager.NewCallbackGroupNoop(),
		cyclemanager.NewCallbackGroupNoop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Shutdown(context.Background())) })

	propName := "txt"
	vFalse := false
	vTrue := true
	class := &models.Class{
		Class: "LikeArrangementSearchableTest",
		Properties: []*models.Property{{
			Name:            propName,
			DataType:        schema.DataTypeText.PropString(),
			Tokenization:    models.PropertyTokenizationWord,
			IndexFilterable: &vFalse,
			IndexSearchable: &vTrue,
		}},
	}

	ctx := context.Background()
	require.NoError(t, store.CreateOrLoadBucket(ctx,
		helpers.BucketSearchableFromPropNameLSM(propName),
		lsmkv.WithStrategy(lsmkv.StrategyMapCollection)))
	b := store.Bucket(helpers.BucketSearchableFromPropNameLSM(propName))
	require.NotNil(t, b)

	rows := map[string][]uint64{
		"a": {1}, "c": {1},
		"abc":   {2},
		"axc":   {3},
		"wild":  {4},
		"card":  {4},
		"chars": {4},
	}
	for key, ids := range rows {
		for _, id := range ids {
			pair := lsmkv.MapPair{Key: make([]byte, 8), Value: make([]byte, 8)}
			binary.BigEndian.PutUint64(pair.Key, id)
			require.NoError(t, b.MapSet([]byte(key), pair))
		}
	}
	require.NoError(t, b.FlushAndSwitch())

	searcher := &Searcher{
		store:  store,
		logger: logger,
		bitmapFactory: roaringset.NewBitmapFactory(
			roaringset.NewBitmapBufPoolNoop(), func() uint64 { return 300_000 }),
		getClass:               func(name string) *models.Class { return class },
		isFallbackToSearchable: func() bool { return false },
		stopwordProvider:       stopwords.NewProvider(fakeStopwordDetector{}, nil),
		shardVersion:           2,
	}

	require.Equal(t, []uint64{1}, likeDocsWith(t, searcher, class, propName, "a.c"))
	require.Equal(t, []uint64{1, 2, 3}, likeDocsWith(t, searcher, class, propName, "a*c"))
	require.Equal(t, []uint64{1, 2, 3}, likeDocsWith(t, searcher, class, propName, "a?c"))
	require.Equal(t, []uint64{4}, likeDocsWith(t, searcher, class, propName, "wild*chars"))
}
