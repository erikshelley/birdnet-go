// sync_state.go tracks which BirdWeather detections have already been imported
// so the download service can resume from a high-water mark and avoid
// re-importing the same detection on every poll cycle.
package birdweather

import (
	"time"

	"github.com/tphakala/birdnet-go/internal/datastore"
	"github.com/tphakala/birdnet-go/internal/errors"
	"gorm.io/gorm"
)

// BirdweatherImport tracks a single detection imported from BirdWeather. It is
// a birdweather-package-owned table, deliberately separate from the core Note
// schema so the download feature can be added or removed without a Note
// migration.
type BirdweatherImport struct {
	ID                  uint      `gorm:"primaryKey"`
	StationID           string    `gorm:"column:station_id;type:varchar(64);not null;index:idx_birdweather_import_station"`
	ExternalDetectionID string    `gorm:"column:external_detection_id;type:varchar(64);not null;uniqueIndex"`
	NoteID              uint      `gorm:"not null"`
	ImportedAt          time.Time `gorm:"index;not null"`
}

// TableName pins the table name so it does not depend on GORM's pluralization
// of the struct name.
func (BirdweatherImport) TableName() string {
	return "birdweather_imports"
}

// importStore persists BirdweatherImport rows through the shared
// datastore.Interface's Transaction accessor, keeping the tracking table
// independent of which datastore backend (v2only or legacy) is active.
type importStore struct {
	db datastore.Interface
}

// newImportStore creates an importStore backed by db.
func newImportStore(db datastore.Interface) *importStore {
	return &importStore{db: db}
}

// migrate creates the birdweather_imports table if it does not already exist.
func (s *importStore) migrate() error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		return tx.AutoMigrate(&BirdweatherImport{})
	})
}

// lastImportedAt returns the most recent ImportedAt recorded for stationID, or
// the zero time if that station has no tracked imports yet (first run).
func (s *importStore) lastImportedAt(stationID string) (time.Time, error) {
	var row BirdweatherImport
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Where("station_id = ?", stationID).Order("imported_at DESC").First(&row)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil
		}
		return result.Error
	})
	if err != nil {
		return time.Time{}, err
	}
	return row.ImportedAt, nil
}

// alreadyImported reports whether externalID has already been recorded.
func (s *importStore) alreadyImported(externalID string) (bool, error) {
	var count int64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&BirdweatherImport{}).Where("external_detection_id = ?", externalID).Count(&count).Error
	})
	return count > 0, err
}

// record inserts a tracking row for a newly imported detection. A duplicate
// externalID fails the unique index; the caller should treat that as
// already-imported (e.g. a racing cycle) rather than a hard error.
func (s *importStore) record(stationID, externalID string, noteID uint, importedAt time.Time) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&BirdweatherImport{
			StationID:           stationID,
			ExternalDetectionID: externalID,
			NoteID:              noteID,
			ImportedAt:          importedAt,
		}).Error
	})
}
