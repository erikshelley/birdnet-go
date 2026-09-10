// download_service.go implements periodic polling of BirdWeather's public
// GraphQL API to download the user's own station detections, so they appear
// alongside local detections. This mirrors the polling/backoff pattern used
// by internal/weather/weather.go's Service.
package birdweather

import (
	"context"
	"runtime/debug"
	"sync"
	"time"

	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/datastore"
	"github.com/tphakala/birdnet-go/internal/datastore/v2/entities"
	"github.com/tphakala/birdnet-go/internal/detection"
	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/logger"
)

var (
	globalDownloadServiceMu sync.RWMutex
	globalDownloadService   *DownloadService
)

// RegisterDownloadService stores the download service instance for
// package-level access (e.g. health/status introspection). Called during
// audio pipeline startup.
func RegisterDownloadService(s *DownloadService) {
	globalDownloadServiceMu.Lock()
	globalDownloadService = s
	globalDownloadServiceMu.Unlock()
}

// UnregisterDownloadService clears the stored download service instance.
func UnregisterDownloadService() {
	globalDownloadServiceMu.Lock()
	globalDownloadService = nil
	globalDownloadServiceMu.Unlock()
}

// DownloadServiceRegistered reports whether a download service is currently
// registered, i.e. detection downloads are enabled and running.
func DownloadServiceRegistered() bool {
	globalDownloadServiceMu.RLock()
	defer globalDownloadServiceMu.RUnlock()
	return globalDownloadService != nil
}

const (
	// downloadDefaultStartupDelay delays the first poll to reduce startup DB
	// contention with other services, mirroring weather.DefaultStartupDelay.
	downloadDefaultStartupDelay = 10 * time.Second

	// downloadDefaultPollIntervalMinutes is used if the configured interval is
	// invalid (StartPolling reads the raw setting and time.NewTicker panics on
	// a non-positive interval).
	downloadDefaultPollIntervalMinutes = 15

	// downloadMaxPagesPerCycle caps how many pages a single poll cycle fetches,
	// so a very large backfill window cannot turn one cycle into an unbounded
	// loop; the remainder is picked up on the next cycle since the sync window
	// start only advances once a detection is actually imported.
	downloadMaxPagesPerCycle = 50

	// Backoff bounds for transient BirdWeather API failures.
	downloadInitialBackoff    = 30 * time.Second
	downloadMaxBackoff        = 30 * time.Minute
	downloadBackoffMultiplier = 2

	// birdweatherSourceNode identifies imported detections as coming from
	// BirdWeather rather than a local audio device. birdweatherAudioSourceIDPrefix
	// and birdweatherDisplayNamePrefix are combined with a station ID so multiple
	// configured stations show up as distinct, filterable sources.
	birdweatherSourceNode          = "birdweather"
	birdweatherAudioSourceIDPrefix = "birdweather:"
	birdweatherDisplayNamePrefix   = "BirdWeather "
	birdweatherModelName           = "birdweather-import"
)

// downloadBackoff tracks consecutive failures and backs off transient errors
// so a repeatedly failing BirdWeather API cannot be hit every poll cycle.
type downloadBackoff struct {
	mu                  sync.Mutex
	consecutiveFailures int
	currentBackoff      time.Duration
	nextAllowed         time.Time
}

// shouldSkip reports whether the backoff window from a previous failure is
// still open.
func (b *downloadBackoff) shouldSkip() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.nextAllowed.IsZero() && time.Now().Before(b.nextAllowed)
}

// reset clears all backoff state after a successful cycle.
func (b *downloadBackoff) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures = 0
	b.currentBackoff = 0
	b.nextAllowed = time.Time{}
}

// recordFailure increments the failure counter and computes the next backoff.
func (b *downloadBackoff) recordFailure() (backoff time.Duration, failures int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures++
	if b.currentBackoff == 0 {
		b.currentBackoff = downloadInitialBackoff
	} else {
		b.currentBackoff *= downloadBackoffMultiplier
	}
	if b.currentBackoff > downloadMaxBackoff {
		b.currentBackoff = downloadMaxBackoff
	}
	b.nextAllowed = time.Now().Add(b.currentBackoff)
	return b.currentBackoff, b.consecutiveFailures
}

// DownloadService periodically downloads the user's own BirdWeather station
// detections and saves them as local detections tagged with source
// "birdweather", so they appear in the dashboard, explore, and patterns pages
// alongside detections from local audio sources.
type DownloadService struct {
	settings *conf.Settings
	repo     datastore.DetectionRepository
	client   *GraphQLClient
	imports  *importStore

	startupDelay time.Duration
	backoff      downloadBackoff

	// fetchMu serializes poll cycles so the StartPolling ticker and an
	// on-demand Poll() call cannot run concurrently.
	fetchMu sync.Mutex
}

// NewDownloadService creates a DownloadService. db is used for the
// birdweather_imports bookkeeping table (via its Transaction accessor); repo
// is used to persist downloaded detections through the same domain-model path
// local detections use. Returns an error if the bookkeeping table cannot be
// created.
func NewDownloadService(settings *conf.Settings, db datastore.Interface, repo datastore.DetectionRepository) (*DownloadService, error) {
	imports := newImportStore(db)
	if err := imports.migrate(); err != nil {
		return nil, errors.New(err).
			Component("birdweather").
			Category(errors.CategoryDatabase).
			Context("operation", "migrate_birdweather_imports").
			Build()
	}

	return &DownloadService{
		settings:     settings,
		repo:         repo,
		client:       NewGraphQLClient(),
		imports:      imports,
		startupDelay: downloadDefaultStartupDelay,
	}, nil
}

// StartPolling starts the detection download polling loop. It blocks until
// stopChan is closed.
func (s *DownloadService) StartPolling(stopChan <-chan struct{}) {
	log := GetLogger()

	// Poll interval is read once at startup, matching weather.Service's
	// StartPolling: the ticker cadence is not hot-reloadable without
	// restarting the whole audio pipeline service. Unlike the upload client
	// (recreated by the reconfigure_birdweather control action), the download
	// poller has no such wiring yet, so toggling Download.Enabled or changing
	// PollIntervalMinutes/BackfillDays via the UI requires an app restart to
	// take effect.
	settings := conf.CurrentOrFallback(s.settings)
	interval := time.Duration(settings.Realtime.Birdweather.Download.PollIntervalMinutes) * time.Minute
	if interval <= 0 {
		log.Warn("Invalid BirdWeather download poll interval, using default",
			logger.Int("configured_minutes", settings.Realtime.Birdweather.Download.PollIntervalMinutes),
			logger.Int("default_minutes", downloadDefaultPollIntervalMinutes))
		interval = downloadDefaultPollIntervalMinutes * time.Minute
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stopChan:
			cancel()
		case <-ctx.Done():
		}
	}()

	log.Info("Starting BirdWeather detection download polling",
		logger.String("interval", interval.String()))

	if s.startupDelay > 0 {
		select {
		case <-time.After(s.startupDelay):
		case <-stopChan:
			return
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.safePoll(ctx)

	for {
		select {
		case <-ticker.C:
			s.safePoll(ctx)
		case <-stopChan:
			log.Info("Stopping BirdWeather detection download polling")
			return
		}
	}
}

// safePoll runs Poll with panic recovery so a panic degrades only the
// download service instead of crashing the whole process.
func (s *DownloadService) safePoll(ctx context.Context) {
	log := GetLogger()
	defer func() {
		if r := recover(); r != nil {
			backoff, failures := s.backoff.recordFailure()
			log.Error("BirdWeather detection download cycle panicked, recovering and backing off",
				logger.Any("panic", r),
				logger.String("backoff", backoff.String()),
				logger.Int("consecutive_failures", failures),
				logger.String("stack", string(debug.Stack())))
		}
	}()
	if err := s.Poll(ctx); err != nil {
		log.Warn("BirdWeather detection download cycle failed", logger.Error(err))
	}
}

// Poll runs a single download cycle across all configured stations: for each
// station it fetches detections newer than that station's last recorded
// import (or the configured backfill window on first run), saves any not
// already imported, and records them. A failure on one station does not stop
// the others; Poll only returns an error (and backs off) if every configured
// station failed. Safe to call directly for on-demand/testing use in addition
// to the StartPolling loop.
func (s *DownloadService) Poll(ctx context.Context) error {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	if s.backoff.shouldSkip() {
		return nil
	}

	settings := conf.CurrentOrFallback(s.settings)
	bw := &settings.Realtime.Birdweather
	if !bw.Download.Enabled || len(bw.Download.StationIDs) == 0 {
		return nil
	}

	log := GetLogger()
	until := time.Now().UTC()

	var (
		totalImported int
		failures      int
		lastErr       error
	)

	for _, stationID := range bw.Download.StationIDs {
		imported, err := s.pollStation(ctx, bw, stationID, until)
		if err != nil {
			failures++
			lastErr = err
			log.Warn("BirdWeather detection download failed for station",
				logger.String("station_id", stationID),
				logger.Error(err))
			continue
		}
		totalImported += imported
	}

	if failures > 0 && failures == len(bw.Download.StationIDs) {
		s.recordFailure(lastErr)
		return lastErr
	}

	s.backoff.reset()
	log.Info("BirdWeather detection download cycle complete",
		logger.Int("imported", totalImported),
		logger.Int("stations", len(bw.Download.StationIDs)),
		logger.Int("failed_stations", failures))
	return nil
}

// pollStation runs a single download cycle for one station.
func (s *DownloadService) pollStation(ctx context.Context, bw *conf.BirdweatherSettings, stationID string, until time.Time) (int, error) {
	since, err := s.syncWindowStart(bw, stationID)
	if err != nil {
		return 0, err
	}
	return s.syncDetections(ctx, bw, stationID, since, until)
}

// recordFailure records a poll cycle failure and logs the resulting backoff.
func (s *DownloadService) recordFailure(err error) {
	backoff, failures := s.backoff.recordFailure()
	GetLogger().Warn("BirdWeather detection download cycle failed, backing off",
		logger.Error(err),
		logger.String("backoff", backoff.String()),
		logger.Int("consecutive_failures", failures))
}

// syncWindowStart determines the start of the period to fetch for stationID:
// that station's last recorded import time, or now minus the configured
// backfill window on first run (calendar-day arithmetic, DST-safe). A
// zero/disabled backfill window on first run starts from now, so nothing is
// backfilled.
func (s *DownloadService) syncWindowStart(bw *conf.BirdweatherSettings, stationID string) (time.Time, error) {
	last, err := s.imports.lastImportedAt(stationID)
	if err != nil {
		return time.Time{}, err
	}
	if !last.IsZero() {
		return last, nil
	}
	now := time.Now().UTC()
	if bw.Download.BackfillDays <= 0 {
		return now, nil
	}
	return now.AddDate(0, 0, -bw.Download.BackfillDays), nil
}

// syncDetections fetches and imports stationID's detections in [since, until],
// paginating until BirdWeather reports no next page or downloadMaxPagesPerCycle
// is reached. Returns the number of detections newly imported.
func (s *DownloadService) syncDetections(ctx context.Context, bw *conf.BirdweatherSettings, stationID string, since, until time.Time) (int, error) {
	var (
		after    string
		imported int
	)

	for range downloadMaxPagesPerCycle {
		result, err := s.client.FetchStationDetections(ctx, stationID, since, until, after)
		if err != nil {
			return imported, err
		}

		for _, det := range result.Detections {
			if det.Confidence < bw.Threshold {
				continue
			}

			already, err := s.imports.alreadyImported(det.ID)
			if err != nil {
				return imported, err
			}
			if already {
				continue
			}

			if err := s.importDetection(ctx, bw, stationID, &det); err != nil {
				GetLogger().Warn("Failed to import BirdWeather detection",
					logger.String("station_id", stationID),
					logger.String("detection_id", det.ID),
					logger.Error(err))
				continue
			}
			imported++
		}

		if !result.PageInfo.HasNextPage {
			break
		}
		after = result.PageInfo.EndCursor
	}

	return imported, nil
}

// importDetection saves a single downloaded detection through the shared
// detection repository and records it in the bookkeeping table, tagged with a
// per-station source identity so multiple stations remain distinguishable.
func (s *DownloadService) importDetection(ctx context.Context, bw *conf.BirdweatherSettings, stationID string, det *StationDetection) error {
	result := &detection.Result{
		Timestamp:  det.Timestamp,
		SourceNode: birdweatherSourceNode,
		AudioSource: detection.AudioSource{
			ID:          birdweatherAudioSourceIDPrefix + stationID,
			Type:        string(entities.SourceTypeBirdWeather),
			DisplayName: birdweatherDisplayNamePrefix + "(" + stationID + ")",
		},
		BeginTime: det.Timestamp,
		EndTime:   det.Timestamp.Add(detectionDurationSeconds * time.Second),
		Species: detection.Species{
			ScientificName: det.Species.ScientificName,
			CommonName:     det.Species.CommonName,
		},
		Confidence: det.Confidence,
		Latitude:   det.Coords.Lat,
		Longitude:  det.Coords.Lon,
		Threshold:  bw.Threshold,
		Model:      detection.ModelInfo{Name: birdweatherModelName}.WithDefaults(),
	}

	if err := s.repo.Save(ctx, result, nil); err != nil {
		return err
	}

	return s.imports.record(stationID, det.ID, result.ID, det.Timestamp)
}
