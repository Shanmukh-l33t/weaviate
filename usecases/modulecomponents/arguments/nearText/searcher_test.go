//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2026 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package nearText

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	enterrors "github.com/weaviate/weaviate/entities/errors"
	"github.com/weaviate/weaviate/entities/moduletools"
)

// failingVectorizer succeeds on the first okCalls calls, then returns err.
type failingVectorizer struct {
	okCalls int
	err     error
}

func (v *failingVectorizer) Texts(context.Context, []string, moduletools.ClassConfig) ([]float32, error) {
	if v.okCalls > 0 {
		v.okCalls--
		return []float32{1, 0}, nil
	}
	return nil, v.err
}

type tenantlessClassConfig struct {
	moduletools.ClassConfig
}

func (tenantlessClassConfig) Tenant() string { return "" }

// TestSearcherKeepsModulePrecondition pins that each vectorization wrap in
// the searcher keeps the module's typed error reachable with errors.As and
// its message unchanged after the label.
func TestSearcherKeepsModulePrecondition(t *testing.T) {
	const msg = "no api key found neither in request header: X-Openai-Api-Key nor in environment variable under OPENAI_APIKEY"

	tests := []struct {
		name    string
		params  *NearTextParams
		okCalls int
		wantMsg string
	}{
		{
			name:    "keywords",
			params:  &NearTextParams{Values: []string{"space"}},
			wantMsg: "vectorize keywords: " + msg,
		},
		{
			name: "move to",
			params: &NearTextParams{
				Values: []string{"space"},
				MoveTo: ExploreMove{Values: []string{"stars"}, Force: 0.5},
			},
			okCalls: 1,
			wantMsg: "vectorize move to: vectorize move to: " + msg,
		},
		{
			name: "move away from",
			params: &NearTextParams{
				Values:       []string{"space"},
				MoveAwayFrom: ExploreMove{Values: []string{"stars"}, Force: 0.5},
			},
			okCalls: 1,
			wantMsg: "vectorize move away from: vectorize move to: " + msg,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vectorizer := &failingVectorizer{okCalls: tt.okCalls, err: enterrors.NewErrModulePrecondition(errors.New(msg))}
			searcher := NewSearcher[[]float32](vectorizer)

			_, err := searcher.VectorSearches()["nearText"](context.Background(), tt.params, "Movie", nil, tenantlessClassConfig{})
			require.Error(t, err)
			require.EqualError(t, err, tt.wantMsg)
			var precondition enterrors.ErrModulePrecondition
			require.ErrorAs(t, err, &precondition)
			require.EqualError(t, precondition, msg)
		})
	}
}
