package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGraftStorageValidate(t *testing.T) {
	t.Parallel()

	valid := GraftStorage{Mode: "shadow", Database: "sqlite:///var/opt/graft/meta.db", Bucket: "file:///var/opt/graft/objects"}

	for _, tc := range []struct {
		desc       string
		storage    GraftStorage
		standalone bool
		err        string
	}{
		{desc: "off by default", storage: GraftStorage{}},
		{desc: "explicitly off ignores the rest", storage: GraftStorage{Mode: "off", Database: "nonsense"}},
		{desc: "shadow", storage: valid},
		{desc: "shadow with postgres", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: "s3://b"}},
		{desc: "postgresql scheme", storage: GraftStorage{Mode: "shadow", Database: "postgresql://u@h/db", Bucket: "mem://"}},
		{desc: "authoritative in standalone mode", storage: GraftStorage{Mode: "authoritative", Database: valid.Database, Bucket: valid.Bucket}, standalone: true},
		{desc: "authoritative needs standalone", storage: GraftStorage{Mode: "authoritative", Database: valid.Database, Bucket: valid.Bucket}, err: "requires graft.standalone"},
		{desc: "unknown mode", storage: GraftStorage{Mode: "primary"}, err: "unknown mode"},
		{desc: "missing database", storage: GraftStorage{Mode: "shadow", Bucket: valid.Bucket}, err: "database is required"},
		{desc: "relative sqlite path", storage: GraftStorage{Mode: "shadow", Database: "sqlite://meta.db", Bucket: valid.Bucket}, err: "unsupported database URL"},
		{desc: "unknown database", storage: GraftStorage{Mode: "shadow", Database: "mysql://h/db", Bucket: valid.Bucket}, err: "unsupported database URL"},
		{desc: "missing bucket", storage: GraftStorage{Mode: "shadow", Database: valid.Database}, err: "bucket is required"},
		{desc: "bucket without scheme", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: "/var/opt/objects"}, err: "bucket must be a URL"},
		{desc: "node id with slash", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, NodeID: "a/b"}, err: "node_id"},
		{desc: "bad sync timeout", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, SyncTimeout: "0s"}, err: "sync_timeout"},
		{desc: "bad shadow timeout", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, ShadowTimeout: "soon"}, err: "shadow_timeout"},
		{desc: "gc times", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "30m", GCMargin: "1m"}},
		{desc: "bad gc_grace", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "-1h"}, err: "gc_grace"},
		{desc: "bad gc_margin", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCMargin: "0"}, err: "gc_margin"},
		{desc: "gc_grace not above sync_timeout + gc_margin", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "6m", GCMargin: "5m", SyncTimeout: "1m"}, err: "gc_grace 6m0s must be greater than sync_timeout 1m0s + gc_margin 5m0s: raise gc_grace or lower sync_timeout"},
		{desc: "a v0.2 configuration with a long sync_timeout", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, SyncTimeout: "1h"}, err: "a v0.2 configuration with sync_timeout of 55m0s or more must set gc_grace"},
		{desc: "gc_grace just above sync_timeout + gc_margin", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GCGrace: "361s", GCMargin: "5m", SyncTimeout: "1m"}},
		{desc: "checkpoints and gc auto in authoritative mode", storage: GraftStorage{Mode: "authoritative", Database: valid.Database, Bucket: valid.Bucket, Checkpoints: "auto", GC: "auto", MaintenanceInterval: "15m", LeaseTTL: "2m", CheckpointChangesets: ptr[uint64](0), CheckpointPackBytes: ptr[int64](0)}, standalone: true},
		{desc: "checkpoints and gc off", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, Checkpoints: "off", GC: "off"}},
		{desc: "checkpoints auto in shadow mode", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, Checkpoints: "auto"}, err: `checkpoints = "auto" requires mode = "authoritative"`},
		{desc: "gc auto in shadow mode", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GC: "auto"}, err: `gc = "auto" requires mode = "authoritative"`},
		{desc: "gc auto with the storage core off", storage: GraftStorage{GC: "auto"}, err: `requires mode = "authoritative"`},
		{desc: "unknown checkpoints", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, Checkpoints: "nightly"}, err: "unknown checkpoints"},
		{desc: "unknown gc", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, GC: "yes"}, err: "unknown gc"},
		{desc: "bad maintenance interval", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, MaintenanceInterval: "0s"}, err: "maintenance_interval"},
		{desc: "bad lease ttl", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, LeaseTTL: "never"}, err: "lease_ttl"},
		{desc: "negative checkpoint pack bytes", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, CheckpointPackBytes: ptr[int64](-1)}, err: "checkpoint_pack_bytes"},
		{desc: "postgres pool", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: valid.Bucket, DatabaseMaxOpenConns: ptr(8), DatabaseMaxIdleConns: ptr(0), DatabaseConnMaxIdleTime: "1m", DatabaseConnMaxLifetime: "1h"}},
		{desc: "pool of a sqlite database", storage: GraftStorage{Mode: "shadow", Database: valid.Database, Bucket: valid.Bucket, DatabaseMaxOpenConns: ptr(8)}, err: "apply to a Postgres database only"},
		{desc: "no open connection", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: valid.Bucket, DatabaseMaxOpenConns: ptr(0)}, err: "database_max_open_conns 0 must be at least 1"},
		{desc: "more idle than open connections", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: valid.Bucket, DatabaseMaxOpenConns: ptr(4), DatabaseMaxIdleConns: ptr(5)}, err: "database_max_idle_conns 5 must be between 0 and database_max_open_conns 4"},
		{desc: "idle connections above the default open connections", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: valid.Bucket, DatabaseMaxIdleConns: ptr(33)}, err: "database_max_idle_conns 33"},
		{desc: "bad idle time", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: valid.Bucket, DatabaseConnMaxIdleTime: "0s"}, err: "database_conn_max_idle_time"},
		{desc: "bad lifetime", storage: GraftStorage{Mode: "shadow", Database: "postgres://u@h/db", Bucket: valid.Bucket, DatabaseConnMaxLifetime: "forever"}, err: "database_conn_max_lifetime"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			err := tc.storage.validate(tc.standalone)
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestGraftStorageDefaults(t *testing.T) {
	t.Parallel()

	var s GraftStorage
	require.False(t, s.Enabled())

	sync, err := s.SyncTimeoutOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageSyncTimeout, sync)

	shadow, err := s.ShadowTimeoutOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageShadowTimeout, shadow)

	grace, err := s.GCGraceOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageGCGrace, grace)

	margin, err := s.GCMarginOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageGCMargin, margin)
	require.NoError(t, s.validateGC(), "the defaults are consistent")

	require.False(t, s.MaintenanceAuto(), "the background maintenance is off with the storage core off")
	interval, err := s.MaintenanceIntervalOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageMaintenanceInterval, interval)
	ttl, err := s.LeaseTTLOrDefault()
	require.NoError(t, err)
	require.Equal(t, DefaultGraftStorageLeaseTTL, ttl)
	require.Equal(t, DefaultGraftStorageCheckpointChangesets, s.CheckpointChangesetsOrDefault())
	require.Equal(t, DefaultGraftStorageCheckpointPackBytes, s.CheckpointPackBytesOrDefault())
	pool, err := s.DatabasePoolOrDefault()
	require.NoError(t, err)
	require.Equal(t, GraftStorageDatabasePool{MaxOpenConns: 32, MaxIdleConns: 32, ConnMaxIdleTime: 5 * time.Minute, ConnMaxLifetime: 30 * time.Minute}, pool)
	pool, err = GraftStorage{DatabaseMaxOpenConns: ptr(8)}.DatabasePoolOrDefault()
	require.NoError(t, err)
	require.Equal(t, 8, pool.MaxIdleConns, "the idle connections follow the open ones")

	s = GraftStorage{Mode: "shadow", SyncTimeout: "250ms"}
	require.True(t, s.Enabled())
	sync, err = s.SyncTimeoutOrDefault()
	require.NoError(t, err)
	require.Equal(t, 250*time.Millisecond, sync)
	require.False(t, s.MaintenanceAuto(), "no checkpoints outside authoritative mode")

	// Authoritative mode checkpoints by default (checkpoints delete nothing);
	// the garbage collection, which deletes, is opt-in.
	s = GraftStorage{Mode: "authoritative"}
	require.True(t, s.CheckpointsAuto())
	require.False(t, s.GCAuto())
	require.True(t, s.MaintenanceAuto())
	s.Checkpoints = "off"
	require.False(t, s.MaintenanceAuto())
	s.GC = "auto"
	require.True(t, s.GCAuto())
	require.True(t, s.MaintenanceAuto())
}

// TestGraftStorageMaintenanceFromTOML checks that a threshold set to 0 in the
// file means never, not the default.
func TestGraftStorageMaintenanceFromTOML(t *testing.T) {
	t.Parallel()

	cfg, err := Load(strings.NewReader(`
[graft.storage]
mode = "authoritative"
gc = "auto"
maintenance_interval = "30m"
checkpoint_changesets = 0
checkpoint_pack_bytes = 1048576
lease_ttl = "3m"
`))
	require.NoError(t, err)

	s := cfg.Graft.Storage
	require.True(t, s.CheckpointsAuto())
	require.True(t, s.GCAuto())
	require.Equal(t, uint64(0), s.CheckpointChangesetsOrDefault())
	require.Equal(t, int64(1<<20), s.CheckpointPackBytesOrDefault())
	interval, err := s.MaintenanceIntervalOrDefault()
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, interval)
	ttl, err := s.LeaseTTLOrDefault()
	require.NoError(t, err)
	require.Equal(t, 3*time.Minute, ttl)
}

func ptr[T any](v T) *T { return &v }

func TestGraftValidateChecksStorage(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, Graft{Storage: GraftStorage{Mode: "authoritative", Database: "sqlite:///x.db", Bucket: "mem://"}}.Validate(), "requires graft.standalone")
	require.NoError(t, Graft{Standalone: true, Storage: GraftStorage{Mode: "authoritative", Database: "sqlite:///x.db", Bucket: "mem://"}}.Validate())
}
