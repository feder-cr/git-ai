// Package open opens the storage core from the [graft.storage] configuration.
// It is separate from corestore so that the implementation packages can
// import corestore without a cycle.
package open

import (
	"context"
	"fmt"
	"os"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/blobstore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// Open returns nil (the storage core is off) when the mode is off, and
// otherwise opens the database and the bucket. The configuration has been
// validated by config.Graft.Validate.
func Open(ctx context.Context, cfg config.GraftStorage) (*corestore.Core, error) {
	mode, err := corestore.ParseMode(cfg.Mode)
	if err != nil {
		return nil, err
	}
	if mode == corestore.ModeOff {
		return nil, nil
	}

	syncTimeout, err := cfg.SyncTimeoutOrDefault()
	if err != nil {
		return nil, err
	}
	shadowTimeout, err := cfg.ShadowTimeoutOrDefault()
	if err != nil {
		return nil, err
	}
	gcGrace, err := cfg.GCGraceOrDefault()
	if err != nil {
		return nil, err
	}
	gcMargin, err := cfg.GCMarginOrDefault()
	if err != nil {
		return nil, err
	}

	nodeID := cfg.NodeID
	if nodeID == "" {
		if nodeID, err = os.Hostname(); err != nil {
			return nil, fmt.Errorf("graft storage: node_id is not set and the host name is unknown: %w", err)
		}
	}

	pool, err := cfg.DatabasePoolOrDefault()
	if err != nil {
		return nil, err
	}

	meta, err := sqlstore.OpenWithPool(ctx, cfg.Database, sqlstore.Pool{
		MaxOpenConns:    pool.MaxOpenConns,
		MaxIdleConns:    pool.MaxIdleConns,
		ConnMaxIdleTime: pool.ConnMaxIdleTime,
		ConnMaxLifetime: pool.ConnMaxLifetime,
	})
	if err != nil {
		return nil, fmt.Errorf("graft storage: open database: %w", err)
	}

	blobs, err := blobstore.Open(ctx, cfg.Bucket)
	if err != nil {
		_ = meta.Close()
		return nil, fmt.Errorf("graft storage: open bucket: %w", err)
	}

	return &corestore.Core{
		Mode:          mode,
		NodeID:        nodeID,
		Meta:          meta,
		Blobs:         blobs,
		SyncTimeout:   syncTimeout,
		ShadowTimeout: shadowTimeout,
		GCGrace:       gcGrace,
		GCMargin:      gcMargin,
	}, nil
}
