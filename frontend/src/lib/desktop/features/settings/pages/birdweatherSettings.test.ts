import { describe, expect, it } from 'vitest';
import { isBirdweatherTestable, parseStationIdsInput } from './birdweatherSettings';

describe('parseStationIdsInput', () => {
  it('splits a comma-separated list and trims whitespace', () => {
    expect(parseStationIdsInput('abc123, def456 , ghi789')).toEqual(['abc123', 'def456', 'ghi789']);
  });

  it('drops blank entries from trailing/repeated commas', () => {
    expect(parseStationIdsInput('abc123,, ,def456,')).toEqual(['abc123', 'def456']);
  });

  it('returns an empty array for empty input', () => {
    expect(parseStationIdsInput('')).toEqual([]);
  });

  it('returns an empty array for whitespace/comma-only input', () => {
    expect(parseStationIdsInput('   ,  ,')).toEqual([]);
  });

  it('preserves a single station ID with no commas', () => {
    expect(parseStationIdsInput('12345')).toEqual(['12345']);
  });
});

describe('isBirdweatherTestable', () => {
  it('is false when neither upload nor download is configured', () => {
    expect(isBirdweatherTestable(undefined)).toBe(false);
    expect(isBirdweatherTestable({})).toBe(false);
  });

  it('is true when upload is enabled with a token, even if download is disabled', () => {
    expect(
      isBirdweatherTestable({
        enabled: true,
        id: 'ABCDEF123456789012345678',
        download: { enabled: false, stationIds: [], pollIntervalMinutes: 15, backfillDays: 0 },
      })
    ).toBe(true);
  });

  it('is false when upload is enabled but has no token', () => {
    expect(
      isBirdweatherTestable({
        enabled: true,
        id: '',
      })
    ).toBe(false);
  });

  it('is true when download is enabled with station IDs, even if upload is disabled', () => {
    expect(
      isBirdweatherTestable({
        enabled: false,
        id: '',
        download: {
          enabled: true,
          stationIds: ['12345'],
          pollIntervalMinutes: 15,
          backfillDays: 0,
        },
      })
    ).toBe(true);
  });

  it('is false when download is enabled but has no station IDs', () => {
    expect(
      isBirdweatherTestable({
        enabled: false,
        download: { enabled: true, stationIds: [], pollIntervalMinutes: 15, backfillDays: 0 },
      })
    ).toBe(false);
  });

  it('is true when both upload and download are independently ready', () => {
    expect(
      isBirdweatherTestable({
        enabled: true,
        id: 'ABCDEF123456789012345678',
        download: {
          enabled: true,
          stationIds: ['12345'],
          pollIntervalMinutes: 15,
          backfillDays: 0,
        },
      })
    ).toBe(true);
  });
});
