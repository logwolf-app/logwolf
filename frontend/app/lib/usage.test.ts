import { describe, expect, it } from 'vitest';

import { formatBytes, formatUsageMonth, quotaShare, wasMeasured } from './usage';

describe('formatBytes', () => {
	it('picks the largest decimal unit the size reaches', () => {
		expect(formatBytes(0, 'en-US')).toBe('0 B');
		expect(formatBytes(999, 'en-US')).toBe('999 B');
		expect(formatBytes(1000, 'en-US')).toBe('1 kB');
		expect(formatBytes(1536, 'en-US')).toBe('1.5 kB');
		expect(formatBytes(20_400_000, 'en-US')).toBe('20 MB');
		expect(formatBytes(3_250_000_000, 'en-US')).toBe('3.3 GB');
	});

	it('stops at the largest unit it knows', () => {
		expect(formatBytes(5e18, 'en-US')).toBe('5,000 PB');
	});
});

describe('wasMeasured', () => {
	it('is false for Go’s zero time', () => {
		expect(wasMeasured('0001-01-01T00:00:00Z')).toBe(false);
		expect(wasMeasured('')).toBe(false);
	});

	it('is true for a real measure', () => {
		expect(wasMeasured('2026-10-06T12:00:00Z')).toBe(true);
	});
});

describe('quotaShare', () => {
	it('is the share of the monthly events, past 1 when over', () => {
		expect(quotaShare(250, 1000)).toBe(0.25);
		expect(quotaShare(1500, 1000)).toBe(1.5);
	});

	it('is undefined for a plan without a monthly limit', () => {
		expect(quotaShare(250, 0)).toBeUndefined();
	});
});

describe('formatUsageMonth', () => {
	it('names the month in UTC', () => {
		expect(formatUsageMonth('2026-10-01T00:00:00Z', 'en-US')).toBe('October 2026');
	});
});
