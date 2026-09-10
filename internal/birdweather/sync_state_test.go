package birdweather

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tphakala/birdnet-go/internal/datastore"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newTestImportStore returns an importStore backed by a fresh in-memory
// SQLite database, migrated and ready for use.
func newTestImportStore(t *testing.T) *importStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)

	store := newImportStore(&datastore.SQLiteStore{DataStore: datastore.DataStore{DB: db}}) //nolint:modernize // keyed literal required: SQLiteStore has unexported fields outside this package
	require.NoError(t, store.migrate())
	return store
}

func TestImportStore_LastImportedAt_EmptyReturnsZero(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	last, err := store.lastImportedAt()
	require.NoError(t, err)
	require.True(t, last.IsZero())
}

func TestImportStore_RecordAndAlreadyImported(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	imported, err := store.alreadyImported("det-1")
	require.NoError(t, err)
	require.False(t, imported)

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.record("det-1", 42, now))

	imported, err = store.alreadyImported("det-1")
	require.NoError(t, err)
	require.True(t, imported)

	last, err := store.lastImportedAt()
	require.NoError(t, err)
	require.WithinDuration(t, now, last, time.Second)
}

func TestImportStore_LastImportedAt_ReturnsMostRecent(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	older := time.Now().Add(-time.Hour).UTC()
	newer := time.Now().UTC()
	require.NoError(t, store.record("det-old", 1, older))
	require.NoError(t, store.record("det-new", 2, newer))

	last, err := store.lastImportedAt()
	require.NoError(t, err)
	require.WithinDuration(t, newer, last, time.Second)
}

func TestImportStore_Record_DuplicateExternalIDFails(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	now := time.Now().UTC()
	require.NoError(t, store.record("dup", 1, now))
	require.Error(t, store.record("dup", 2, now))
}
