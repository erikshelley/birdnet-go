/**
 * Pure logic for the BirdWeather integration settings page, extracted for
 * unit testing independent of the Svelte component tree. Uploads and
 * detection downloads are independent features (see plan.md's "Design
 * revision"): neither requires the other to be enabled/configured.
 */
import type { BirdWeatherSettings } from '$lib/stores/settings';

/**
 * Parses a comma-separated station IDs input into a clean array: trims each
 * entry and drops blanks (e.g. from trailing commas or repeated whitespace).
 */
export function parseStationIdsInput(value: string): string[] {
  return value
    .split(',')
    .map(entry => entry.trim())
    .filter(Boolean);
}

/**
 * Reports whether the BirdWeather "Test Connection" button should be
 * enabled: as soon as uploads OR downloads are sufficiently configured to
 * attempt, independent of the other.
 */
export function isBirdweatherTestable(bw: Partial<BirdWeatherSettings> | undefined): boolean {
  if (!bw) return false;
  const uploadReady = Boolean(bw.enabled && bw.id);
  const download = bw.download;
  const downloadReady = Boolean(download && download.enabled && download.stationIds.length);
  return uploadReady || downloadReady;
}
