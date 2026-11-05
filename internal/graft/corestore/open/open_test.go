package open

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestDefaultPoolOfTheConfiguration checks that a configuration without pool settings opens the
// database with sqlstore.DefaultPool, which documents the choice of the defaults.
func TestDefaultPoolOfTheConfiguration(t *testing.T) {
	pool, err := config.GraftStorage{}.DatabasePoolOrDefault()
	require.NoError(t, err)
	require.Equal(t, sqlstore.DefaultPool, sqlstore.Pool{
		MaxOpenConns:    pool.MaxOpenConns,
		MaxIdleConns:    pool.MaxIdleConns,
		ConnMaxIdleTime: pool.ConnMaxIdleTime,
		ConnMaxLifetime: pool.ConnMaxLifetime,
	})
}
