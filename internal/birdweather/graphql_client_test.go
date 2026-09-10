package birdweather

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestGraphQLClient builds a GraphQLClient pointed at a test server.
func newTestGraphQLClient(server *httptest.Server) *GraphQLClient {
	return &GraphQLClient{
		httpClient: server.Client(),
		endpoint:   server.URL,
	}
}

// jsonHandler responds with the given graphQLResponse envelope, marshaling
// data from the provided value.
func jsonHandler(t *testing.T, data any, gqlErrors []graphQLError) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		var raw json.RawMessage
		if data != nil {
			b, err := json.Marshal(data)
			assert.NoError(t, err)
			raw = b
		}
		body, err := json.Marshal(graphQLResponse{Data: raw, Errors: gqlErrors})
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func TestGraphQLClient_VerifyStation_Success(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(jsonHandler(t, stationVerifyResponse{
		Station: &StationInfo{ID: "4", Name: "Test Station"},
	}, nil))
	defer server.Close()

	client := newTestGraphQLClient(server)
	name, err := client.VerifyStation(t.Context(), "4")
	require.NoError(t, err)
	assert.Equal(t, "Test Station", name)
}

func TestGraphQLClient_VerifyStation_EmptyStationID(t *testing.T) {
	t.Parallel()
	client := newTestGraphQLClient(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no HTTP request should be made for an empty station ID")
		w.WriteHeader(http.StatusInternalServerError)
	})))

	_, err := client.VerifyStation(t.Context(), "")
	require.Error(t, err)
}

func TestGraphQLClient_VerifyStation_StationNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(jsonHandler(t, stationVerifyResponse{Station: nil}, nil))
	defer server.Close()

	client := newTestGraphQLClient(server)
	_, err := client.VerifyStation(t.Context(), "does-not-exist")
	require.Error(t, err)
}

func TestGraphQLClient_VerifyStation_GraphQLErrorSurfaced(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(jsonHandler(t, nil, []graphQLError{{Message: "station id is invalid"}}))
	defer server.Close()

	client := newTestGraphQLClient(server)
	_, err := client.VerifyStation(t.Context(), "bad-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "station id is invalid")
}

func TestGraphQLClient_VerifyStation_HTTPErrorStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newTestGraphQLClient(server)
	_, err := client.VerifyStation(t.Context(), "4")
	require.Error(t, err)
}

func TestGraphQLClient_VerifyStation_MalformedJSON(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not valid json"))
	}))
	defer server.Close()

	client := newTestGraphQLClient(server)
	_, err := client.VerifyStation(t.Context(), "4")
	require.Error(t, err)
}

func TestGraphQLClient_FetchStationDetections_EmptyStationID(t *testing.T) {
	t.Parallel()
	client := newTestGraphQLClient(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no HTTP request should be made for an empty station ID")
		w.WriteHeader(http.StatusInternalServerError)
	})))

	_, err := client.FetchStationDetections(t.Context(), "", time.Now(), time.Now(), "")
	require.Error(t, err)
}

func TestGraphQLClient_FetchStationDetections_StationNotFound(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(jsonHandler(t, stationDetectionsResponse{Station: nil}, nil))
	defer server.Close()

	client := newTestGraphQLClient(server)
	_, err := client.FetchStationDetections(t.Context(), "missing", time.Now(), time.Now(), "")
	require.Error(t, err)
}

func TestGraphQLClient_FetchStationDetections_MapsEdgesAndPageInfo(t *testing.T) {
	t.Parallel()
	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(jsonHandler(t, stationDetectionsResponse{
		Station: &stationDetectionsStation{
			Detections: stationDetectionsConnection{
				Edges: []stationDetectionEdge{
					{Node: StationDetection{
						ID:         "d1",
						Timestamp:  ts,
						Confidence: 0.87,
						Species:    StationDetectionSpecies{CommonName: "Blue Jay", ScientificName: "Cyanocitta cristata"},
						Coords:     StationDetectionCoords{Lat: 1.5, Lon: 2.5},
					}},
				},
				PageInfo: PageInfo{HasNextPage: true, EndCursor: "cursor-1"},
			},
		},
	}, nil))
	defer server.Close()

	client := newTestGraphQLClient(server)
	page, err := client.FetchStationDetections(t.Context(), "4", ts.Add(-24*time.Hour), ts, "")
	require.NoError(t, err)
	require.Len(t, page.Detections, 1)
	assert.Equal(t, "d1", page.Detections[0].ID)
	assert.Equal(t, "Blue Jay", page.Detections[0].Species.CommonName)
	assert.InDelta(t, 0.87, page.Detections[0].Confidence, 0.0001)
	assert.True(t, page.PageInfo.HasNextPage)
	assert.Equal(t, "cursor-1", page.PageInfo.EndCursor)
}

func TestGraphQLClient_FetchStationDetections_SendsPeriodAndCursorVariables(t *testing.T) {
	t.Parallel()
	var capturedBody graphQLRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
		resp, err := json.Marshal(stationDetectionsResponse{
			Station: &stationDetectionsStation{Detections: stationDetectionsConnection{PageInfo: PageInfo{}}},
		})
		assert.NoError(t, err)
		body, err := json.Marshal(graphQLResponse{Data: resp})
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client := newTestGraphQLClient(server)
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	_, err := client.FetchStationDetections(t.Context(), "4", from, to, "prev-cursor")
	require.NoError(t, err)

	assert.Equal(t, "4", capturedBody.Variables["id"])
	assert.Equal(t, "prev-cursor", capturedBody.Variables["after"])
	period, ok := capturedBody.Variables["period"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "2025-01-01", period["from"])
	assert.Equal(t, "2025-01-02", period["to"])
}

func TestGraphQLClient_FetchStationDetections_OmitsAfterWhenEmpty(t *testing.T) {
	t.Parallel()
	var capturedBody graphQLRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&capturedBody))
		resp, err := json.Marshal(stationDetectionsResponse{
			Station: &stationDetectionsStation{Detections: stationDetectionsConnection{PageInfo: PageInfo{}}},
		})
		assert.NoError(t, err)
		body, err := json.Marshal(graphQLResponse{Data: resp})
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	client := newTestGraphQLClient(server)
	_, err := client.FetchStationDetections(t.Context(), "4", time.Now(), time.Now(), "")
	require.NoError(t, err)

	_, hasAfter := capturedBody.Variables["after"]
	assert.False(t, hasAfter, "after should be omitted from variables when empty")
}

func TestGraphQLClient_Do_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(jsonHandler(t, stationVerifyResponse{Station: &StationInfo{ID: "4"}}, nil))
	defer server.Close()

	client := newTestGraphQLClient(server)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := client.VerifyStation(ctx, "4")
	require.Error(t, err)
}

func TestNewGraphQLClient_UsesPublicEndpoint(t *testing.T) {
	t.Parallel()
	client := NewGraphQLClient()
	assert.Equal(t, graphQLEndpoint, client.endpoint)
	assert.NotNil(t, client.httpClient)
}
