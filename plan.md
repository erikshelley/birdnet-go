# Plan: BirdWeather Detection Download Integration

## Decisions (from user Q&A)

- Scope: only the user's own BirdWeather station(s) (the configured station ID), not public/nearby stations.
- Audio: metadata only, no soundscape/audio clip download.
- Dedup: no merge with local detections; always keep as distinct records tagged with source="birdweather".
- Sync: periodic polling (mirrors internal/weather/weather.go pattern), not GraphQL subscriptions/websocket.
- Backfill: support configurable historical backfill window on first enable (not forward-only).
- PR scope: land the whole feature as one PR (one cohesive concern per AGENTS.md's PR rule). If maintainers deem it too large during review, split into follow-up PRs at that point rather than pre-splitting now.

## Design revision (post-implementation, during review)

- **Upload and download station IDs are independent, and download supports multiple stations.** Original decision reused the single upload `ID` field for both directions; this was wrong. Real use cases: (1) upload-only to one's own station, (2) download-only from one or more other stations to merge into local detections, (3) both simultaneously with _different_ IDs (e.g. download from several stations, upload the aggregate elsewhere). Fix: `BirdweatherDownloadSettings` gets its own `StationIDs []string`, independent of the top-level `ID` (which remains upload-only). Download no longer depends on upload being enabled at all (UI previously nested the whole download subsection inside the upload-enabled fieldset - that was a bug, not a design requirement; backend validation was already independent).
- Per-station sync bookkeeping: `BirdweatherImport` gets a `StationID` column so each configured station tracks its own high-water mark independently (stations may be added/removed or backfilled at different times).
- Per-station `AudioSource`: each station gets a distinct source id/display name (`birdweather:<stationID>`) so multiple stations show up as distinct, filterable sources rather than being merged into one generic "BirdWeather" source.
- `TestConnection`/`TestBirdWeatherConnection` now run upload stages only if upload is enabled, and the (now per-station-list) "Station Read Access" stage only if download is enabled - independently, not one gated on the other.

## Key research findings

- BirdWeather has a public GraphQL API at https://app.birdweather.com/graphql (no auth token model documented in our client; station ID used in upload URLs may or may not equal the GraphQL station ID - NEEDS VERIFICATION at implementation time via a `station(id: ID!) { id name }` test query).
- Relevant GraphQL query: `station(id) { detections(period: {from, to}, first, after) { edges { node { id timestamp confidence probability species { commonName scientificName } coords { lat lon } } cursor } pageInfo { hasNextPage endCursor } } }`.
- Existing upload client: internal/birdweather/birdweather_client.go (BwClient, Publish/UploadSoundscape/PostDetection/TestConnection). No read/GET code exists yet.
- Core detection save path (reusable for imports): `DetectionRepository.Save(ctx, result *detection.Result, additionalResults []detection.AdditionalResult) error` in internal/datastore/repository.go / internal/datastore/detection_repository.go. Building a `detection.Result` with SourceNode="birdweather", AudioSource{ID,Type:"birdweather",DisplayName}, Species, Confidence, Timestamp, Lat/Lon and calling Save() is enough to make it show up in dashboard/explore/patterns - NO API v2 changes needed since:
  - internal/api/v2/detections/detections.go already supports `source` filter and returns `SourceInfo{id,type,displayName}`.
  - internal/datastore/search_advanced.go `AdvancedSearchFilters.Source` already resolves against audio_sources / source_node.
  - Analytics/patterns source filter already implemented in frontend (AnalyticsControlBar.svelte, charts declare `supports.source`).
- v2 datastore (default) normalizes source via internal/datastore/v2/entities/audio_source.go `AudioSource{SourceURI, NodeName, SourceType, DisplayName}` with `SourceType` enum (rtsp/alsa/pulseaudio/file/unknown) - needs a new constant e.g. `SourceTypeBirdWeather = "birdweather"` (varchar column, no migration needed for enum value).
- Config: internal/conf/config.go `BirdweatherSettings` (Enabled, Debug, ID, Threshold, LocationAccuracy, RetrySettings) at line ~343. Validated in internal/conf/validate_services.go (~line 504).
- Polling template: internal/weather/weather.go `Service` (Start/Stop/StartPolling/Poll, backoff state, fetchMu, panic recovery). Wired from internal/analysis/audio_pipeline_service.go `startWeatherPolling()` (~line 2082), called via `p.wg.Go(...)`, using `p.done` stop channel, with `weather.RegisterService`/`UnregisterService` package-level accessors.
- Frontend BirdWeather settings UI: frontend/src/lib/desktop/features/settings/pages/IntegrationSettingsPage.svelte (toggle, ID field, threshold slider, test button hitting POST /api/v2/integrations/birdweather/test with SSE-like multi-stage response via MultiStageOperation component).
- Frontend Detection type: frontend/src/lib/types/detection.types.ts has `source?: SourceInfo | null` already wired end-to-end (Dashboard/Explore/Patterns).
- Dashboard: frontend/src/lib/desktop/features/dashboard/pages/DashboardPage.svelte. Explore: frontend/src/lib/desktop/features/detections/DetectionsPage.svelte. Patterns: frontend/src/lib/desktop/features/analytics/pages/ActivityPage.svelte + AnalyticsControlBar.svelte + registry/charts.ts.

## Steps

### Phase 1: BirdWeather GraphQL read client (backend)

1. Add `internal/birdweather/graphql_client.go`: minimal POST-based GraphQL client (JSON body `{query, variables}` to `https://app.birdweather.com/graphql`), reusing existing secure HTTP client/circuit breaker style from birdweather_client.go. Implement `FetchStationDetections(ctx, stationID string, from, to time.Time, after string) (page StationDetectionsPage, err error)` and a lightweight `VerifyStation(ctx, stationID string) (name string, err error)` for connection testing.
2. Define minimal Go structs mirroring the GraphQL `Detection`/`Species`/`Coordinates`/`PageInfo` shapes actually needed (id, timestamp, confidence, species.commonName/scientificName, coords.lat/lon).
3. Use `internal/errors` (never stdlib `errors`) for all error paths, and the existing `GetLogger()`/structured `logger` package pattern from birdweather_client.go for logging. No magic numbers - name constants for timeouts, page size, etc.
4. This client makes outbound requests on the user's behalf, putting it in SECURITY.md's stated scope: verify TLS (no `InsecureSkipVerify`), enforce request timeouts, and never log credentials/PII (consistent with the existing upload client's practices).

### Phase 2: Config & validation (backend, _depends on nothing, parallel with Phase 1_)

5. Extend `BirdweatherSettings` in internal/conf/config.go with a `Download` sub-struct: `Enabled bool`, `PollIntervalMinutes int` (default 15, min 5), `BackfillDays int` (default 0 = disabled, max cap e.g. 90), reuse existing `Threshold` for minimum confidence filter (no new field).
6. Add validation rules in internal/conf/validate_services.go (interval/backfill bounds) near existing Birdweather validation (~line 504).
7. Regenerate config.schema.json via existing `cmd/gen-schema` tool.
8. Update PRIVACY.md's "Optional External Integrations" BirdWeather subsection to document the new download direction (what's fetched - the user's own previously-uploaded species/confidence/timestamp/coords, opt-in/disabled by default, own station only, no other users'/stations' data accessed) - required by CONTRIBUTING.md's "document any external services in PRIVACY.md" rule.

### Phase 3: Sync state & download service (backend, _depends on Phase 1+2_)

9. Add a small new table for idempotency/high-water-mark tracking (avoid touching core `Note` schema): `BirdweatherImport{ID, ExternalDetectionID string uniqueIndex, NoteID uint, ImportedAt time.Time}` in internal/birdweather (own migration) or internal/datastore. Register AutoMigrate in both internal/datastore/v2/manager.go (default path) and legacy datastore init if still supported - verify which is active by default and migrate accordingly.
10. Add `internal/birdweather/download_service.go`: `DownloadService` struct mirroring internal/weather/weather.go's `Service` (fetchMu, backoff state, panic-recovering `StartPolling(stopChan)`, `Poll(ctx) error`, `Name()/Start()/Stop()`).
    - Read `Enabled`/`PollIntervalMinutes` from the current settings snapshot on each cycle (mirror `weather.Service`'s hot-reload behavior) - never capture these at construction time only.
    - Determine sync window: `since` = last `ImportedAt` from BirdweatherImport (or `now - BackfillDays` if empty/first run, capped).
    - Call GraphQL client, paginate via `after` cursor, filter by `Confidence >= Threshold`.
    - For each detection not already present (check BirdweatherImport unique constraint), build `detection.Result{Timestamp, SourceNode:"birdweather", AudioSource:{ID:"birdweather:"+stationID, Type:"birdweather", DisplayName:"BirdWeather"}, Species, Confidence, Latitude, Longitude, Model:{Name:"birdweather-import"}}` and call `DetectionRepository.Save(ctx, &result, nil)`, then insert the BirdweatherImport tracking row (same transaction if repository supports it).
    - Use `internal/errors` and the structured `logger` package throughout; named constants for poll bounds/page size/backoff caps.
11. Add `entities.SourceType` constant `SourceTypeBirdWeather` in internal/datastore/v2/entities/audio_source.go (or equivalent v1 mapping) so the source resolves/display cleanly.

### Phase 4: Lifecycle wiring (backend, _depends on Phase 3_)

12. Add `startBirdWeatherDownloadPolling(metrics)` in internal/analysis/audio_pipeline_service.go mirroring `startWeatherPolling()` (~line 2082): construct service (return early/no-op if `Download.Enabled` false), `p.wg.Go(func(){ svc.StartPolling(p.done) })`, package-level Register/Unregister for introspection.
13. Call the new start function from the same place `startWeatherPolling` is invoked.

### Phase 5: API v2 (backend, _depends on Phase 1_, parallel with Phase 3/4)

14. Extend internal/api/v2/integrations/integrations.go test-connection handler (or add a sibling handler) to also validate station read access via `VerifyStation` - surfaced through the existing multi-stage test flow the frontend already calls.
15. Update `internal/api/v2/README.md`'s endpoint table immediately for the extended/added handler - mandatory per internal/api/v2/CLAUDE.md's critical rules.
16. No changes needed to `/api/v2/detections`, analytics/source endpoints - already source-aware.

### Phase 6: Frontend settings UI (_depends on Phase 2 field names being final_)

17. Extend frontend/src/lib/desktop/features/settings/pages/IntegrationSettingsPage.svelte BirdWeather section with a "Download detections" subsection: enable toggle, poll interval field, backfill days field; wire through existing `hasChanged`/settings store pattern used for the upload settings.
18. Update the BirdWeather settings TypeScript interface (wherever `BirdWeatherSettings` type lives, referenced by IntegrationSettingsPage) to add the `download` sub-object matching backend JSON tags - no `any` types (frontend/CLAUDE.md critical rule).
19. Check frontend/src/lib/desktop/features/dashboard (SourceBadge component or equivalent) for any hardcoded source-type-to-icon mapping; add a case for `type: "birdweather"` if such a mapping exists, so it renders distinctly instead of falling back to a generic/unknown icon.
20. Add all new user-facing strings (toggle label, field labels/help text, any status/error text) to `frontend/static/messages/en.json` first, run `npm run i18n:sync` to propagate to all 15 locale files, then `npm run generate:i18n-types` and commit the generated types - mandatory per frontend/CLAUDE.md i18n workflow (CI-checked).

### Phase 7: Tests & verification

21. Go: unit tests for graphql_client.go (mock HTTP server), download_service.go (poll cycle, backoff, backfill window, dedup via BirdweatherImport), conf validation tests. Use testify, `-race`, table-driven tests, avoid `time.Sleep` (prefer `testing/synctest` or a fakeable clock), per TESTING.md and internal/CLAUDE.md.
22. Frontend: extend settings page test coverage for the new fields (npm test).
23. Manual/E2E: use `~/src/birdnet-go-qa` harness - settings round-trip test extension if needed.
24. Run the mandatory preflight quality gate (`.agents/skills/preflight/SKILL.md`, all 6 review passes) before pushing, and include a "Preflight Status" section in the PR description per AGENTS.md.

## Relevant files

- `internal/birdweather/birdweather_client.go` — existing upload client, pattern reference for HTTP client setup/circuit breaker.
- `internal/birdweather/graphql_client.go` — new, GraphQL read client.
- `internal/birdweather/download_service.go` — new, polling service.
- `internal/conf/config.go` — extend `BirdweatherSettings`.
- `internal/conf/validate_services.go` — add validation for new fields.
- `internal/datastore/repository.go` / `internal/datastore/detection_repository.go` — reuse `Save()`.
- `internal/datastore/v2/entities/audio_source.go` — add `SourceTypeBirdWeather`.
- `internal/analysis/audio_pipeline_service.go` — wire polling lifecycle (`startWeatherPolling` as template).
- `internal/api/v2/integrations/integrations.go` — extend test-connection handler.
- `internal/api/v2/README.md` — document the extended/new endpoint.
- `PRIVACY.md` — document the new download data flow.
- `frontend/src/lib/desktop/features/settings/pages/IntegrationSettingsPage.svelte` — new UI fields.
- `frontend/src/lib/types/detection.types.ts` — confirm no changes needed (already source-aware).
- `frontend/static/messages/en.json` (+ synced locales, generated i18n types) — new UI strings.

## Verification

1. `go test -race ./internal/birdweather/... ./internal/conf/... ./internal/analysis/...`
2. `golangci-lint run -v`
3. `npm run check:all` and `npm test` in frontend/
4. Manual: enable download in Settings > Integrations, confirm test-connection succeeds, confirm detections appear in Dashboard recent list, Explore page (filterable by source=birdweather), and Patterns/Analytics charts with source filter set to BirdWeather.
5. Confirm hot-reload: toggling enabled/interval takes effect without restart (per repo hot-reload settings rule).

## Further Considerations

1. **Station ID mismatch risk**: the upload `ID` (used in REST upload URL) may differ from the GraphQL `station(id)` identifier. Recommend implementing `VerifyStation` as a preflight check in the test-connection flow; if it fails, surface a clear error and consider adding a separate `DownloadStationID` override field (deferred unless verification proves they differ).
2. **Confidence filter field reuse**: reusing existing `Threshold` field for import filtering (recommended, avoids new field) vs. a dedicated `DownloadMinConfidence` (more explicit but duplicates a concept). Recommend reuse for minimalism unless user wants independent thresholds for upload vs download.
3. **BirdWeather API rate limits/ToS**: no documented rate limit; recommend a sane floor on poll interval (e.g. minimum 5 minutes) and modest page size to be a good API citizen.
