import Logwolf, { LogwolfEvent } from '@logwolf/client-js';

/**
 * The dashboard's own telemetry client, or null when `API_KEY` is unset or
 * blank. The key is made on the Keys page, so a fresh install has to boot
 * without one; callers skip capturing when there is no client. A key that is
 * set but malformed still throws, as the SDK validates it.
 */
export function logwolfFromEnv(env: Record<string, string | undefined> = process.env): Logwolf | null {
	const apiKey = env.API_KEY?.trim();
	if (!apiKey) return null;

	return new Logwolf({
		apiKey,
		url: env.API_URL!,
		sampleRate: 0.5,
		errorSampleRate: 1,
		flushIntervalMs: 10_000,
		maxBatchSize: 100,
		maxQueueSize: 1000,
		requestTimeoutMs: 500,
		retryDelaysMs: [1000, 2000, 5000],
	});
}

export const logwolf = logwolfFromEnv();

export function injectRequest(ev: LogwolfEvent, request: Request) {
	ev.set('request', {
		cache: request.cache,
		credentials: request.credentials,
		destination: request.destination,
		headers: Object.fromEntries(request.headers.entries()),
		integrity: request.integrity,
		keepalive: request.keepalive,
		method: request.method,
		mode: request.mode,
		referrer: request.referrer,
		redirect: request.redirect,
		url: request.url,
	});
}

export function injectResponse(ev: LogwolfEvent, response: Response) {
	ev.set('response', {
		headers: Object.fromEntries(response.headers.entries()),
		status: response.status,
		statusText: response.statusText,
		type: response.type,
		url: response.url,
	});
}
