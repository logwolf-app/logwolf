import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { commitSession, getSession } from './session.server';
import { unsealToken } from './token.server';

// auth.server reads the allowlist when it is loaded, so each test loads it
// afresh with the environment it sets.
async function loadAuth() {
	vi.resetModules();
	return import('./auth.server');
}

type Broker = { status?: number; calls: { headers: Headers; body: unknown }[] };

// GitHub, as sign-in talks to it: the code exchange, then the user. And the
// broker, which records the sign-in: it answers `status`, and keeps each call.
function stubGitHub(login: string, broker: Broker = { calls: [] }) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = String(input);
			if (url.endsWith('/login/oauth/access_token')) return Response.json({ access_token: 'gho_signed_in' });
			if (url.endsWith('/user')) {
				return Response.json({ id: 583231, login, name: 'Someone', avatar_url: 'https://x/a.png', email: null });
			}
			if (url.includes('/user/orgs')) return Response.json([]);
			if (url === 'http://broker/users/me' && init?.method === 'PUT') {
				const body = JSON.parse(String(init.body));
				broker.calls.push({ headers: new Headers(init.headers), body });
				const status = broker.status ?? 200;
				return Response.json(
					{ error: status >= 400, message: 'm', data: { id: 'u1', github_id: body.github_id, github_login: login } },
					{ status },
				);
			}
			return new Response(null, { status: 404 });
		}),
	);
	return broker;
}

describe('handleGitHubCallback', () => {
	beforeEach(() => {
		vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_USERS', 'octocat');
		vi.stubEnv('API_URL', 'http://broker/');
		vi.stubEnv('INTERNAL_API_SECRET', 'internal-secret');
	});
	afterEach(() => {
		vi.unstubAllGlobals();
		vi.unstubAllEnvs();
		vi.restoreAllMocks();
	});

	it('keeps the GitHub token in the session, sealed', async () => {
		stubGitHub('Octocat');
		const { handleGitHubCallback } = await loadAuth();

		const res = (await handleGitHubCallback('code', new Request('http://localhost/auth'))) as Response;

		expect(res.headers.get('Location')).toBe('/dashboard');
		const cookie = res.headers.get('Set-Cookie')!.split(';')[0]!;
		const session = await getSession(cookie);
		expect(session.get('githubUser')?.login).toBe('Octocat');
		expect(unsealToken(session.get('githubToken'))).toBe('gho_signed_in');

		// The cookie is signed, not encrypted: its payload is readable, the token is not.
		const payload = decodeURIComponent(cookie.split('=')[1]!);
		expect(Buffer.from(payload.split('.')[0]!, 'base64').toString('utf8')).not.toContain('gho_signed_in');
	});

	it('records the sign-in with the broker and keeps the GitHub user ID next to the login', async () => {
		const broker = stubGitHub('Octocat');
		const { handleGitHubCallback } = await loadAuth();

		const res = (await handleGitHubCallback('code', new Request('http://localhost/auth'))) as Response;

		// The login goes as the signed-in user, in GitHub's casing; a private email as none.
		expect(broker.calls).toHaveLength(1);
		expect(broker.calls[0]!.headers.get('X-User-Login')).toBe('Octocat');
		expect(broker.calls[0]!.body).toEqual({ github_id: 583231, email: '' });

		const session = await getSession(res.headers.get('Set-Cookie')!.split(';')[0]!);
		expect(session.get('githubUser')).toMatchObject({ id: 583231, login: 'Octocat' });
	});

	it('keeps nothing for a user the allowlist refuses, and records nothing', async () => {
		const broker = stubGitHub('stranger');
		const { handleGitHubCallback } = await loadAuth();

		const err = await handleGitHubCallback('code', new Request('http://localhost/auth')).catch((e: unknown) => e);

		expect((err as Response).headers.get('Location')).toBe('/auth?error=unauthorized');
		expect((err as Response).headers.get('Set-Cookie')).toBeNull();
		expect(broker.calls).toHaveLength(0);
	});

	it('does not sign in a user it cannot record', async () => {
		stubGitHub('Octocat', { status: 500, calls: [] });
		vi.spyOn(console, 'error').mockImplementation(() => {});
		const { handleGitHubCallback } = await loadAuth();

		const err = await handleGitHubCallback('code', new Request('http://localhost/auth')).catch((e: unknown) => e);

		expect((err as Response).headers.get('Location')).toBe('/auth?error=unavailable');
		expect((err as Response).headers.get('Set-Cookie')).toBeNull();
	});
});

describe('requireAuth', () => {
	it('returns the signed-in user', async () => {
		const { requireAuth } = await loadAuth();
		const session = await getSession();
		session.set('githubUser', { id: 583231, login: 'Octocat', name: 'Someone', avatarUrl: '' });
		const cookie = (await commitSession(session)).split(';')[0]!;

		await expect(requireAuth(new Request('http://localhost/', { headers: { Cookie: cookie } }))).resolves.toMatchObject(
			{
				id: 583231,
				login: 'Octocat',
			},
		);
	});

	// Sessions from before sign-in kept the ID have only the login.
	it('sends a session without the user ID back to sign in', async () => {
		const { requireAuth } = await loadAuth();
		const session = await getSession();
		session.set('githubUser', { login: 'Octocat', name: 'Someone', avatarUrl: '' } as never);
		const cookie = (await commitSession(session)).split(';')[0]!;

		const err = await requireAuth(new Request('http://localhost/', { headers: { Cookie: cookie } })).catch(
			(e: unknown) => e,
		);

		expect((err as Response).headers.get('Location')).toBe('/auth');
	});
});
