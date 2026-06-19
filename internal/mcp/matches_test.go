package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestChunkHitsToMatches_ordersByScoreAndMapsOffsets(t *testing.T) {
	t.Parallel()

	preprocessed := "Subject: Hello\n\nFirst paragraph about budgets.\n\nSecond paragraph."
	body := "First paragraph about budgets.\n\nSecond paragraph."
	prefixRunes := subjectPrefixRuneCount("Hello")

	hits := []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: prefixRunes, ChunkCharEnd: prefixRunes + 28, Score: 0.9},
		{ChunkIndex: 1, ChunkCharStart: prefixRunes + 30, ChunkCharEnd: prefixRunes + 50, Score: 0.7},
	}

	matches, truncated := chunkHitsToMatches(preprocessed, body, prefixRunes, hits, 0, 5, 300)
	require.Len(t, matches, 2)
	require.NotNil(t, matches[0].Score)
	assert.InDelta(t, 0.9, *matches[0].Score, 0.001)
	assert.Contains(t, matches[0].Snippet, "budget")
	assert.Equal(t, 0, matches[0].CharOffset)
	assert.False(t, truncated)
}

func TestChunkHitsToMatches_minScoreAndTruncation(t *testing.T) {
	t.Parallel()

	body := "alpha beta gamma delta"
	hits := []vector.ChunkHit{
		{ChunkIndex: 0, ChunkCharStart: 0, ChunkCharEnd: 5, Score: 0.2},
		{ChunkIndex: 1, ChunkCharStart: 6, ChunkCharEnd: 10, Score: 0.8},
		{ChunkIndex: 2, ChunkCharStart: 11, ChunkCharEnd: 16, Score: 0.6},
	}

	matches, truncated := chunkHitsToMatches(body, body, 0, hits, 0.5, 1, 300)
	require.Len(t, matches, 1)
	assert.InDelta(t, 0.8, *matches[0].Score, 0.001)
	assert.True(t, truncated)
}

func TestExtractContextMatches_keywordShape(t *testing.T) {
	t.Parallel()

	body := "Line one\nLine two with TARGET here\nLine three"
	matches := extractContextMatches(body, []string{"TARGET"}, 80)
	require.NotEmpty(t, matches)
	assert.Contains(t, matches[0].Snippet, "TARGET")
	assert.Greater(t, matches[0].CharOffset, 0)
	assert.Equal(t, 2, matches[0].Line)
	assert.Nil(t, matches[0].Score)
}
