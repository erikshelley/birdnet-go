package birdweather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/tphakala/birdnet-go/internal/conf"
	"github.com/tphakala/birdnet-go/internal/datastore"
	"github.com/tphakala/birdnet-go/internal/datastore/mocks"
	"github.com/tphakala/birdnet-go/internal/detection"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// fakeStationServer serves canned stationDetectionsResponse pages in order,
// one per request, so tests can exercise pagination deterministically.
type fakeStationServer struct {
	mu    sync.Mutex
	calls int
	pages []stationDetectionsResponse
}

func newFakeStationServer(t *testing.T, pages ...stationDetectionsResponse) *httptest.Server {
	t.Helper()
	f := &fakeStationServer{pages: pages}
	return httptest.NewServer(http.HandlerFunc(f.handle))
}

func (f *fakeStationServer) handle(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	idx := f.calls
	f.calls++
	f.mu.Unlock()

	var page stationDetectionsResponse
	if idx < len(f.pages) {
		page = f.pages[idx]
	}
	data, err := json.Marshal(page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(graphQLResponse{Data: data})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// detNode builds a StationDetection fixture for tests.
func detNode(id string, ts time.Time, confidence float64) StationDetection {
	return StationDetection{
		ID:         id,
		Timestamp:  ts,
		Confidence: confidence,
		Species: StationDetectionSpecies{
			CommonName:     "Test Bird",
			ScientificName: "Testus birdus",
		},
		Coords: StationDetectionCoords{Lat: 1.23, Lon: 4.56},
	}
}

// singlePageResponse wraps detections into a one-page, no-next-page response.
func singlePageResponse(detections ...StationDetection) stationDetectionsResponse {
	edges := make([]stationDetectionEdge, 0, len(detections))
	for _, d := range detections {
		edges = append(edges, stationDetectionEdge{Node: d})
	}
	return stationDetectionsResponse{
		Station: &stationDetectionsStation{
			Detections: stationDetectionsConnection{
				Edges:    edges,
				PageInfo: PageInfo{HasNextPage: false},
			},
		},
	}
}

// newTestDownloadService builds a DownloadService wired to a fake GraphQL
// server, an in-memory bookkeeping store, and the given detection repository.
func newTestDownloadService(t *testing.T, server *httptest.Server, repo datastore.DetectionRepository, settings *conf.Settings) *DownloadService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)

	imports := newImportStore(&datastore.SQLiteStore{DataStore: datastore.DataStore{DB: db}}) //nolint:modernize // keyed literal required: SQLiteStore has unexported fields outside this package
	require.NoError(t, imports.migrate())

	return &DownloadService{
		settings: settings,
		repo:     repo,
		imports:  imports,
		client: &GraphQLClient{
			httpClient: server.Client(),
			endpoint:   server.URL,
		},
	}
}

func testSettings(stationID string, enabled bool, backfillDays int, threshold float64) *conf.Settings {
	settings := &conf.Settings{}
	settings.Realtime.Birdweather = conf.BirdweatherSettings{
		ID:        stationID,
		Threshold: threshold,
		Download: conf.BirdweatherDownloadSettings{
			Enabled:      enabled,
			BackfillDays: backfillDays,
		},
	}
	return settings
}

func TestDownloadService_Poll_DisabledIsNoop(t *testing.T) {
	t.Parallel()
	server := newFakeStationServer(t, singlePageResponse(detNode("d1", time.Now(), 0.9)))
	defer server.Close()

	repo := mocks.NewMockDetectionRepository(t)
	settings := testSettings("station-1", false, 0, 0.5)
	svc := newTestDownloadService(t, server, repo, settings)

	require.NoError(t, svc.Poll(t.Context()))
	repo.AssertNotCalled(t, "Save", mock.Anything, mock.Anything, mock.Anything)
}

func TestDownloadService_Poll_MissingStationIDErrors(t *testing.T) {
	t.Parallel()
	server := newFakeStationServer(t, singlePageResponse())
	defer server.Close()

	repo := mocks.NewMockDetectionRepository(t)
	settings := testSettings("", true, 0, 0.5)
	svc := newTestDownloadService(t, server, repo, settings)

	require.Error(t, svc.Poll(t.Context()))
}

func TestDownloadService_Poll_ImportsNewDetectionsAboveThreshold(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	server := newFakeStationServer(t, singlePageResponse(
		detNode("above-threshold", now, 0.9),
		detNode("below-threshold", now, 0.1),
	))
	defer server.Close()

	repo := mocks.NewMockDetectionRepository(t)
	repo.EXPECT().
		Save(mock.Anything, mock.MatchedBy(func(r *detection.Result) bool {
			return r.SourceNode == birdweatherSourceNode && r.Species.CommonName == "Test Bird"
		}), mock.Anything).
		Run(func(_ context.Context, result *detection.Result, _ []detection.AdditionalResult) {
			result.ID = 100
		}).
		Return(nil).
		Once()

	settings := testSettings("station-1", true, 1, 0.5)
	svc := newTestDownloadService(t, server, repo, settings)

	require.NoError(t, svc.Poll(t.Context()))

	imported, err := svc.imports.alreadyImported("above-threshold")
	require.NoError(t, err)
	require.True(t, imported)

	skipped, err := svc.imports.alreadyImported("below-threshold")
	require.NoError(t, err)
	require.False(t, skipped)
}

func TestDownloadService_Poll_SkipsAlreadyImportedDetections(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	server := newFakeStationServer(t,
		singlePageResponse(detNode("dup", now, 0.9)),
		singlePageResponse(detNode("dup", now, 0.9)),
	)
	defer server.Close()

	repo := mocks.NewMockDetectionRepository(t)
	repo.EXPECT().
		Save(mock.Anything, mock.Anything, mock.Anything).
		Run(func(_ context.Context, result *detection.Result, _ []detection.AdditionalResult) {
			result.ID = 1
		}).
		Return(nil).
		Once() // only the first Poll should save; the second must skip it

	settings := testSettings("station-1", true, 1, 0.5)
	svc := newTestDownloadService(t, server, repo, settings)

	require.NoError(t, svc.Poll(t.Context()))
	require.NoError(t, svc.Poll(t.Context()))
}

func TestDownloadService_SyncWindowStart_FirstRunUsesBackfill(t *testing.T) {
	t.Parallel()
	server := newFakeStationServer(t, singlePageResponse())
	defer server.Close()

	repo := mocks.NewMockDetectionRepository(t)
	settings := testSettings("station-1", true, 7, 0.5)
	svc := newTestDownloadService(t, server, repo, settings)

	start, err := svc.syncWindowStart(&settings.Realtime.Birdweather)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().UTC().AddDate(0, 0, -7), start, time.Minute)
}

func TestDownloadService_SyncWindowStart_NoBackfillStartsNow(t *testing.T) {
	t.Parallel()
	server := newFakeStationServer(t, singlePageResponse())
	defer server.Close()

	repo := mocks.NewMockDetectionRepository(t)
	settings := testSettings("station-1", true, 0, 0.5)
	svc := newTestDownloadService(t, server, repo, settings)

	start, err := svc.syncWindowStart(&settings.Realtime.Birdweather)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().UTC(), start, time.Minute)
}

func TestDownloadServiceRegistry(t *testing.T) {
	// Not t.Parallel(): exercises the package-level registry global.
	require.False(t, DownloadServiceRegistered())

	svc := &DownloadService{}
	RegisterDownloadService(svc)
	require.True(t, DownloadServiceRegistered())

	UnregisterDownloadService()
	require.False(t, DownloadServiceRegistered())
}
