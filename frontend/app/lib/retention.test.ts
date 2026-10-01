import { describe, expect, it } from 'vitest';

import { lowersRetention, retentionLabel, retentionOptions } from './retention';

describe('lowersRetention', () => {
	it('is true for a shorter window', () => {
		expect(lowersRetention(90, 30)).toBe(true);
		expect(lowersRetention(365, 180)).toBe(true);
	});

	it('treats forever as the longest window', () => {
		expect(lowersRetention(0, 365)).toBe(true);
		expect(lowersRetention(365, 0)).toBe(false);
		expect(lowersRetention(0, 0)).toBe(false);
	});

	it('is false for a longer or unchanged window', () => {
		expect(lowersRetention(30, 90)).toBe(false);
		expect(lowersRetention(90, 90)).toBe(false);
	});
});

describe('retentionLabel', () => {
	it('names forever and a number of days', () => {
		expect(retentionLabel(0)).toBe('Forever');
		expect(retentionLabel(90)).toBe('90 days');
	});
});

describe('retentionOptions', () => {
	it('lists the choices, in the order the broker gave them', () => {
		expect(retentionOptions(90, [0, 30, 60, 90, 180, 365])).toEqual([0, 30, 60, 90, 180, 365]);
	});

	it('keeps a current value the choices no longer offer, so it still shows', () => {
		expect(retentionOptions(365, [7, 30])).toEqual([365, 7, 30]);
	});

	it('does not change the choices it was given', () => {
		const choices = [30, 60];
		retentionOptions(90, choices);
		expect(choices).toEqual([30, 60]);
	});
});
