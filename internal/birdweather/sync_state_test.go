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

	last, err := store.lastImportedAt("station-1")
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
	require.NoError(t, store.record("station-1", "det-1", 42, now))

	imported, err = store.alreadyImported("det-1")
	require.NoError(t, err)
	require.True(t, imported)

	last, err := store.lastImportedAt("station-1")
	require.NoError(t, err)
	require.WithinDuration(t, now, last, time.Second)
}

func TestImportStore_LastImportedAt_ReturnsMostRecent(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	older := time.Now().Add(-time.Hour).UTC()
	newer := time.Now().UTC()
	require.NoError(t, store.record("station-1", "det-old", 1, older))
	require.NoError(t, store.record("station-1", "det-new", 2, newer))

	last, err := store.lastImportedAt("station-1")
	require.NoError(t, err)
	require.WithinDuration(t, newer, last, time.Second)
}

func TestImportStore_LastImportedAt_IsScopedPerStation(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	stationAImported := time.Now().Add(-48 * time.Hour).UTC()
	stationBImported := time.Now().UTC()
	require.NoError(t, store.record("station-a", "det-a", 1, stationAImported))
	require.NoError(t, store.record("station-b", "det-b", 2, stationBImported))

	lastA, err := store.lastImportedAt("station-a")
	require.NoError(t, err)
	require.WithinDuration(t, stationAImported, lastA, time.Second)

	lastB, err := store.lastImportedAt("station-b")
	require.NoError(t, err)
	require.WithinDuration(t, stationBImported, lastB, time.Second)
}

func TestImportStore_AlreadyImportedSet_ReturnsOnlyKnownIDs(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	now := time.Now().UTC()
	require.NoError(t, store.record("station-1", "known-1", 1, now))
	require.NoError(t, store.record("station-1", "known-2", 2, now))

	set, err := store.alreadyImportedSet([]string{"known-1", "known-2", "unknown-3"})
	require.NoError(t, err)
	require.Len(t, set, 2)
	_, ok := set["known-1"]
	require.True(t, ok)
	_, ok = set["known-2"]
	require.True(t, ok)
	_, ok = set["unknown-3"]
	require.False(t, ok)
}

func TestImportStore_AlreadyImportedSet_EmptyInputReturnsEmpty(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	set, err := store.alreadyImportedSet(nil)
	require.NoError(t, err)
	require.Empty(t, set)
}

func TestImportStore_Record_DuplicateExternalIDFails(t *testing.T) {
	t.Parallel()
	store := newTestImportStore(t)

	now := time.Now().UTC()
	require.NoError(t, store.record("station-1", "dup", 1, now))
	require.Error(t, store.record("station-1", "dup", 2, now))
}
