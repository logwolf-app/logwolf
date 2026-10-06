import Logwolf from '@logwolf/client-js';
import { describe, expect, it } from 'vitest';

import { logwolfFromEnv } from './logwolf';

const url = 'http://broker/';

describe('logwolfFromEnv', () => {
	it('has no client when API_KEY is unset or blank, so the dashboard boots without one', () => {
		expect(logwolfFromEnv({ API_URL: url })).toBeNull();
		expect(logwolfFromEnv({ API_URL: url, API_KEY: '' })).toBeNull();
		expect(logwolfFromEnv({ API_URL: url, API_KEY: '   ' })).toBeNull();
	});

	it('builds a client from API_KEY', () => {
		expect(logwolfFromEnv({ API_URL: url, API_KEY: 'lw_0123456789abcdef' })).toBeInstanceOf(Logwolf);
	});

	it('trims API_KEY', () => {
		expect(logwolfFromEnv({ API_URL: url, API_KEY: ' lw_0123456789abcdef\n' })).toBeInstanceOf(Logwolf);
	});

	it('refuses a key that is set but malformed rather than run without telemetry', () => {
		expect(() => logwolfFromEnv({ API_URL: url, API_KEY: 'not-a-key' })).toThrow();
	});
});
