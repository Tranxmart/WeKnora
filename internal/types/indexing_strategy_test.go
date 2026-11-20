package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExistingKnowledgeBaseDoesNotEnableWikiByDefault(t *testing.T) {
	for _, value := range []interface{}{
		nil,
		[]byte(`{"vector_enabled":true,"keyword_enabled":true}`),
		[]byte(`{"vector_enabled":true,"keyword_enabled":true,"wiki_enabled":false}`),
	} {
		var strategy IndexingStrategy
		require.NoError(t, strategy.Scan(value))
		kb := KnowledgeBase{Type: KnowledgeBaseTypeDocument, IndexingStrategy: strategy}
		kb.EnsureDefaults()
		assert.False(t, kb.IsWikiEnabled())
		assert.True(t, kb.IndexingStrategy.VectorEnabled)
		assert.True(t, kb.IndexingStrategy.KeywordEnabled)
	}
}
