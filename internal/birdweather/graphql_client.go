// graphql_client.go implements a minimal, read-only client for BirdWeather's
// public GraphQL API (https://app.birdweather.com/graphql), used to download
// the user's own station detections. This is intentionally separate from
// birdweather_client.go, which only handles the upload (soundscape/detection
// publish) side of the integration.
package birdweather

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tphakala/birdnet-go/internal/errors"
	"github.com/tphakala/birdnet-go/internal/httpclient"
	"github.com/tphakala/birdnet-go/internal/logger"
)

const (
	// graphQLEndpoint is BirdWeather's public GraphQL API endpoint.
	graphQLEndpoint = "https://app.birdweather.com/graphql"

	// graphQLRequestTimeout bounds each GraphQL request so a slow or unresponsive
	// endpoint cannot hang the download poll cycle.
	graphQLRequestTimeout = 30 * time.Second

	// maxGraphQLResponseBytes caps how much of a GraphQL response body is read,
	// guarding against a misbehaving or malicious endpoint returning unbounded data.
	maxGraphQLResponseBytes = 10 << 20 // 10 MiB

	// defaultDetectionPageSize is the page size requested for paginated detection
	// queries; kept modest to be a good citizen of BirdWeather's API.
	defaultDetectionPageSize = 100
)

// stationVerifyQuery resolves a station ID to its display name, used as a
// lightweight preflight check that the configured ID is valid for reads.
const stationVerifyQuery = `query verifyStation($id: ID!) {
  station(id: $id) {
    id
    name
  }
}`

// stationDetectionsQuery fetches one page of a station's detections within a
// period, ordered by BirdWeather's default sort, paginating via a cursor.
const stationDetectionsQuery = `query stationDetections($id: ID!, $period: InputDuration, $first: Int, $after: String) {
  station(id: $id) {
    id
    name
    detections(period: $period, first: $first, after: $after) {
      edges {
        cursor
        node {
          id
          timestamp
          confidence
          probability
          species {
            commonName
            scientificName
          }
          coords {
            lat
            lon
          }
        }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`

// StationInfo is the minimal station identity returned by VerifyStation.
type StationInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// StationDetectionSpecies identifies the species of a downloaded detection.
type StationDetectionSpecies struct {
	CommonName     string `json:"commonName"`
	ScientificName string `json:"scientificName"`
}

// StationDetectionCoords is the geographic location of a downloaded detection.
type StationDetectionCoords struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// StationDetection is a single detection returned by the BirdWeather GraphQL API.
type StationDetection struct {
	ID          string                  `json:"id"`
	Timestamp   time.Time               `json:"timestamp"`
	Confidence  float64                 `json:"confidence"`
	Probability *float64                `json:"probability"`
	Species     StationDetectionSpecies `json:"species"`
	Coords      StationDetectionCoords  `json:"coords"`
}

// PageInfo mirrors the GraphQL PageInfo type used for cursor-based pagination.
type PageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// StationDetectionsPage is one page of a station's detections.
type StationDetectionsPage struct {
	Detections []StationDetection
	PageInfo   PageInfo
}

// graphQLRequest is the JSON envelope BirdWeather's GraphQL endpoint expects.
type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// graphQLError represents a single entry in a GraphQL response's "errors" array.
type graphQLError struct {
	Message string `json:"message"`
}

// graphQLResponse is the generic envelope returned by BirdWeather's GraphQL
// endpoint; Data is decoded into a caller-supplied target after error checking.
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors,omitempty"`
}

// stationVerifyResponse decodes the "data" field of a stationVerifyQuery response.
type stationVerifyResponse struct {
	Station *StationInfo `json:"station"`
}

// stationDetectionsResponse decodes the "data" field of a stationDetectionsQuery response.
type stationDetectionsResponse struct {
	Station *struct {
		Detections struct {
			Edges []struct {
				Node StationDetection `json:"node"`
			} `json:"edges"`
			PageInfo PageInfo `json:"pageInfo"`
		} `json:"detections"`
	} `json:"station"`
}

// GraphQLClient is a minimal read-only client for BirdWeather's public GraphQL
// API, used to download the user's own station detections.
type GraphQLClient struct {
	httpClient *http.Client
	endpoint   string
}

// NewGraphQLClient creates a GraphQLClient using an SSRF-guarded HTTP client
// bounded by graphQLRequestTimeout. The endpoint is fixed to BirdWeather's
// public GraphQL API and is not user-configurable.
func NewGraphQLClient() *GraphQLClient {
	return &GraphQLClient{
		httpClient: httpclient.NewGuardedHTTPClient(graphQLRequestTimeout),
		endpoint:   graphQLEndpoint,
	}
}

// VerifyStation checks that stationID resolves to a real BirdWeather station
// and returns its display name. Used as a preflight check before enabling
// detection downloads, since the station ID used for uploads may not be the
// same identifier the GraphQL API expects.
func (c *GraphQLClient) VerifyStation(ctx context.Context, stationID string) (string, error) {
	if stationID == "" {
		return "", errors.Newf("birdweather station ID is required").
			Component("birdweather").
			Category(errors.CategoryValidation).
			Build()
	}

	var resp stationVerifyResponse
	if err := c.do(ctx, stationVerifyQuery, map[string]any{"id": stationID}, &resp); err != nil {
		return "", err
	}
	if resp.Station == nil {
		return "", errors.Newf("birdweather station %q not found", stationID).
			Component("birdweather").
			Category(errors.CategoryNotFound).
			Build()
	}
	return resp.Station.Name, nil
}

// FetchStationDetections retrieves one page of a station's detections between
// from and to (inclusive), paginating forward from the given cursor. Pass an
// empty after to fetch the first page.
func (c *GraphQLClient) FetchStationDetections(ctx context.Context, stationID string, from, to time.Time, after string) (StationDetectionsPage, error) {
	if stationID == "" {
		return StationDetectionsPage{}, errors.Newf("birdweather station ID is required").
			Component("birdweather").
			Category(errors.CategoryValidation).
			Build()
	}

	variables := map[string]any{
		"id": stationID,
		"period": map[string]any{
			"from": from.UTC().Format(time.DateOnly),
			"to":   to.UTC().Format(time.DateOnly),
		},
		"first": defaultDetectionPageSize,
	}
	if after != "" {
		variables["after"] = after
	}

	var resp stationDetectionsResponse
	if err := c.do(ctx, stationDetectionsQuery, variables, &resp); err != nil {
		return StationDetectionsPage{}, err
	}
	if resp.Station == nil {
		return StationDetectionsPage{}, errors.Newf("birdweather station %q not found", stationID).
			Component("birdweather").
			Category(errors.CategoryNotFound).
			Build()
	}

	page := StationDetectionsPage{
		Detections: make([]StationDetection, 0, len(resp.Station.Detections.Edges)),
		PageInfo:   resp.Station.Detections.PageInfo,
	}
	for _, edge := range resp.Station.Detections.Edges {
		page.Detections = append(page.Detections, edge.Node)
	}
	return page, nil
}

// do executes a GraphQL query against the BirdWeather API and decodes the
// "data" field of the response into out (which may be nil to discard it).
func (c *GraphQLClient) do(ctx context.Context, query string, variables map[string]any, out any) error {
	log := GetLogger()

	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return errors.Newf("marshal birdweather graphql request: %w", err).
			Component("birdweather").
			Category(errors.CategoryValidation).
			Build()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.Newf("build birdweather graphql request: %w", err).
			Component("birdweather").
			Category(errors.CategoryNetwork).
			Build()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	log.Debug("Sending BirdWeather GraphQL request", logger.String("endpoint", c.endpoint))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return handleNetworkError(err, c.endpoint, graphQLRequestTimeout, "graphql query")
	}
	defer closeResponseBody(resp)

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxGraphQLResponseBytes))
	if err != nil {
		return errors.Newf("read birdweather graphql response: %w", err).
			Component("birdweather").
			Category(errors.CategoryNetwork).
			Build()
	}

	if resp.StatusCode != http.StatusOK {
		return errors.Newf("birdweather graphql request failed with status %d", resp.StatusCode).
			Component("birdweather").
			Category(errors.CategoryHTTP).
			Context("status_code", resp.StatusCode).
			Build()
	}

	var envelope graphQLResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return errors.Newf("decode birdweather graphql response: %w", err).
			Component("birdweather").
			Category(errors.CategoryValidation).
			Build()
	}

	if len(envelope.Errors) > 0 {
		return errors.Newf("birdweather graphql error: %s", envelope.Errors[0].Message).
			Component("birdweather").
			Category(errors.CategoryIntegration).
			Build()
	}

	if out == nil || len(envelope.Data) == 0 {
		return nil
	}

	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return errors.Newf("decode birdweather graphql data: %w", err).
			Component("birdweather").
			Category(errors.CategoryValidation).
			Build()
	}

	return nil
}
