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

package rest

import (
	"net/http"
	"testing"

	"github.com/go-openapi/runtime/middleware"
	"github.com/stretchr/testify/require"

	aggregateops "github.com/weaviate/weaviate/adapters/handlers/rest/operations/aggregate"
	searchops "github.com/weaviate/weaviate/adapters/handlers/rest/operations/search"
	restsearch "github.com/weaviate/weaviate/adapters/handlers/rest/search"
	enterrors "github.com/weaviate/weaviate/entities/errors"
)

// TestSearchErrRespondersTooManyRequests pins that every search operation,
// aggregate included, answers a 429 with its declared generated responder.
func TestSearchErrRespondersTooManyRequests(t *testing.T) {
	tests := []struct {
		name    string
		respond func(*restsearch.APIError) middleware.Responder
		want    middleware.Responder
	}{
		{name: "aggregate", respond: aggregateErrResponder, want: &aggregateops.AggregateTooManyRequests{}},
		{name: "bm25", respond: searchBm25ErrResponder, want: &searchops.SearchBm25TooManyRequests{}},
		{name: "hybrid", respond: searchHybridErrResponder, want: &searchops.SearchHybridTooManyRequests{}},
		{name: "near-object", respond: searchNearObjectErrResponder, want: &searchops.SearchNearObjectTooManyRequests{}},
		{name: "near-text", respond: searchNearTextErrResponder, want: &searchops.SearchNearTextTooManyRequests{}},
		{name: "near-vector", respond: searchNearVectorErrResponder, want: &searchops.SearchNearVectorTooManyRequests{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := tt.respond(&restsearch.APIError{Status: http.StatusTooManyRequests, Err: enterrors.NewErrRateLimit()})
			require.IsType(t, tt.want, resp)
			code, body := statusOf(t, resp)
			require.Equal(t, http.StatusTooManyRequests, code)
			require.Len(t, body.Error, 1)
			require.Contains(t, body.Error[0].Message, "429 Too many requests")
		})
	}
}
