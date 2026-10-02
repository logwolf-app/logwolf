import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createApi } from '~/lib/api';
import { context, fakeApi, get, location, post, project, sessionCookie, sessionSet, thrown } from '~/test/routes';

import { action, loader } from './index';

vi.mock('~/lib/api', async (importOriginal) => ({
	...(await importOriginal<typeof import('~/lib/api')>()),
	createApi: vi.fn(),
}));

const owned = 'aaaaaaaaaaaaaaaaaaaaaaa1';
const joined = 'bbbbbbbbbbbbbbbbbbbbbbb2';
const outside = 'ccccccccccccccccccccccc3';

function send(id: string, fields: Record<string, string>, cookie: string, opts?: { csrf?: string | null }) {
	return action({ request: post(`/projects/${id}/settings`, fields, cookie, opts), params: { id }, context } as never);
}

describe('/projects/:id/settings', () => {
	let api: ReturnType<typeof fakeApi>;
	let cookie: string;

	beforeEach(async () => {
		api = fakeApi({
			getProjects: async () => [project(owned, 'owner', 'Owned'), project(joined, 'member', 'Joined')],
			updateProject: async () => project(owned),
			updateRetention: async (_id, days) => ({ days, choices: [0, 30, 60, 90, 180, 365] }),
			addMember: async () => {},
			updateMemberRole: async () => {},
			removeMember: async () => {},
			deleteProject: async () => {},
			getMembers: async () => [],
		});
		vi.mocked(createApi).mockReturnValue(api);
		cookie = await sessionCookie({ currentProjectID: owned });
	});

	afterEach(() => {
		vi.unstubAllGlobals();
		vi.unstubAllEnvs();
	});

	it('sends a project the user does not belong to back to /projects', async () => {
		const err = await thrown(
			loader({ request: get(`/projects/${outside}/settings`, cookie), params: { id: outside }, context } as never),
		);
		expect(location(err)).toBe('/projects');

		expect(location(await thrown(send(outside, { intent: 'rename', name: 'Mine now' }, cookie)))).toBe('/projects');
		expect(api.updateProject).not.toHaveBeenCalled();
	});

	it('refuses a form without the session’s CSRF token', async () => {
		const err = await thrown(send(owned, { intent: 'rename', name: 'X' }, cookie, { csrf: 'forged' }));
		expect(err).toMatchObject({ init: { status: 403 } });
		expect(api.updateProject).not.toHaveBeenCalled();
	});

	it.each<Record<string, string>>([
		{ intent: 'rename', name: 'Renamed' },
		{ intent: 'add-member', login: 'someone', role: 'member' },
		{ intent: 'change-role', member: 'ddddddddddddddddddddddd4', login: 'someone', role: 'owner' },
		{ intent: 'remove-member', member: 'ddddddddddddddddddddddd4', login: 'someone' },
		{ intent: 'delete', confirmation: 'Joined' },
	])('lets only an owner $intent', async (fields) => {
		expect(await send(joined, fields, cookie)).toEqual({ error: 'Only an owner can change this.' });
		for (const m of ['updateProject', 'addMember', 'updateMemberRole', 'removeMember', 'deleteProject'] as const) {
			expect(api[m]).not.toHaveBeenCalled();
		}
	});

	describe('retention', () => {
		it('loads the choices the broker offers the project', async () => {
			api.getRetention.mockResolvedValue({ days: 30, choices: [7, 30] });

			const data = await loader({
				request: get(`/projects/${owned}/settings`, cookie),
				params: { id: owned },
				context,
			} as never);

			expect(data).toMatchObject({ days: 30, choices: [7, 30] });
			expect(api.getRetention).toHaveBeenCalledWith(owned);
		});

		it('lets a member raise it, but not lower it', async () => {
			api.getRetention.mockResolvedValue({ days: 90, choices: [0, 30, 60, 90, 180, 365] });

			expect(await send(joined, { intent: 'retention', days: '30' }, cookie)).toEqual({
				error: 'Only an owner can lower retention.',
			});
			expect(api.updateRetention).not.toHaveBeenCalled();

			expect(await send(joined, { intent: 'retention', days: '0' }, cookie)).toEqual({ success: 'Retention updated.' });
			expect(api.updateRetention).toHaveBeenCalledWith(joined, 0);
		});

		it('lets an owner lower it without looking it up first', async () => {
			expect(await send(owned, { intent: 'retention', days: '30' }, cookie)).toEqual({ success: 'Retention updated.' });
			expect(api.updateRetention).toHaveBeenCalledWith(owned, 30);
			expect(api.getRetention).not.toHaveBeenCalled();
		});
	});

	describe('add-member', () => {
		// GitHub's public API, as checkInvitee asks it: one known user, one org.
		function stubGitHub() {
			vi.stubGlobal(
				'fetch',
				vi.fn(async (input: RequestInfo | URL) => {
					const path = new URL(String(input)).pathname;
					if (path === '/users/octodog') return Response.json({ id: 9001, login: 'OctoDog', type: 'User' });
					if (path === '/users/acme') return Response.json({ login: 'Acme', type: 'Organization' });
					return new Response(null, { status: 404 });
				}),
			);
		}

		it('refuses a login GitHub does not know, or an organization', async () => {
			stubGitHub();

			expect(await send(owned, { intent: 'add-member', login: 'nobody', role: 'member' }, cookie)).toEqual({
				error: 'There is no GitHub user named nobody.',
			});
			expect(await send(owned, { intent: 'add-member', login: 'acme', role: 'member' }, cookie)).toMatchObject({
				error: expect.stringMatching(/organization/),
			});
			expect(api.addMember).not.toHaveBeenCalled();
		});

		it('adds someone the allowlist does not clear, under GitHub’s casing, with a warning', async () => {
			stubGitHub();
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_USERS', 'octocat');
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_ORGS', '');

			const res = await send(owned, { intent: 'add-member', login: 'octodog', role: 'owner' }, cookie);

			expect(api.addMember).toHaveBeenCalledWith(owned, { id: 9001, login: 'OctoDog' }, 'owner');
			expect(res).toMatchObject({
				success: 'Added OctoDog as owner.',
				warning: expect.stringMatching(/cannot sign in/),
			});
		});

		// Asked as the owner, GitHub shows an allowed org's private members,
		// whom the public check took for strangers.
		it('clears a private member of an allowed org, asking GitHub as the owner', async () => {
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_USERS', '');
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_ORGS', 'acme');
			const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
				const path = new URL(String(input)).pathname;
				if (path === '/users/octodog') return Response.json({ id: 9001, login: 'OctoDog', type: 'User' });
				const asOwner = new Headers(init?.headers).get('Authorization') === 'Bearer gho_owner';
				if (path === '/orgs/acme/members/OctoDog') return new Response(null, { status: asOwner ? 204 : 302 });
				return new Response(null, { status: 404 });
			});
			vi.stubGlobal('fetch', fetchMock);

			const withToken = await sessionCookie({ currentProjectID: owned, githubToken: 'gho_owner' });
			const res = await send(owned, { intent: 'add-member', login: 'octodog', role: 'member' }, withToken);

			expect(res).toEqual({ success: 'Added OctoDog as member.', warning: undefined });

			// A session from before tokens were kept still works, and warns as before.
			const res2 = await send(owned, { intent: 'add-member', login: 'octodog', role: 'member' }, cookie);
			expect(res2).toMatchObject({ warning: expect.stringMatching(/privately/) });
		});

		it('adds an allowlisted user without a warning', async () => {
			stubGitHub();
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_USERS', 'octodog');

			const res = await send(owned, { intent: 'add-member', login: 'octodog', role: 'member' }, cookie);

			expect(res).toEqual({ success: 'Added OctoDog as member.', warning: undefined });
		});

		// The membership belongs to the GitHub user ID, which only GitHub's answer
		// gives: without it there is nobody to add.
		it('adds nobody when GitHub cannot say who the login is', async () => {
			vi.stubGlobal(
				'fetch',
				vi.fn(async () => new Response(null, { status: 503 })),
			);

			const res = await send(owned, { intent: 'add-member', login: 'octodog', role: 'member' }, cookie);

			expect(res).toEqual({ error: 'Could not reach GitHub to look up octodog. Try again in a moment.' });
			expect(api.addMember).not.toHaveBeenCalled();
		});
	});

	describe('members', () => {
		const member = 'ddddddddddddddddddddddd4';

		it('loads the members, and the signed-in user by ID and login', async () => {
			api.getMembers.mockResolvedValue([]);
			api.getRetention.mockResolvedValue({ days: 90, choices: [90] });

			const data = await loader({
				request: get(`/projects/${owned}/settings`, cookie),
				params: { id: owned },
				context,
			} as never);

			expect(data).toMatchObject({ currentUser: { id: 583231, login: 'Octocat' } });
		});

		it('changes a role by the membership’s id', async () => {
			const res = await send(owned, { intent: 'change-role', member, login: 'octodog', role: 'owner' }, cookie);

			expect(api.updateMemberRole).toHaveBeenCalledWith(owned, member, 'owner');
			expect(res).toEqual({ success: 'octodog is now an owner.' });
		});

		it('removes by the membership’s id', async () => {
			const res = await send(owned, { intent: 'remove-member', member, login: 'octodog' }, cookie);

			expect(api.removeMember).toHaveBeenCalledWith(owned, member);
			expect(res).toEqual({ success: 'Removed octodog.' });
		});
	});

	describe('delete', () => {
		it('needs the project’s exact name', async () => {
			expect(await send(owned, { intent: 'delete', confirmation: 'owned' }, cookie)).toEqual({
				error: 'The project name does not match.',
			});
			expect(api.deleteProject).not.toHaveBeenCalled();
		});

		it('deletes, and takes the project out of the session when it was the current one', async () => {
			const res = await send(owned, { intent: 'delete', confirmation: 'Owned' }, cookie);

			expect(api.deleteProject).toHaveBeenCalledWith(owned);
			expect(location(res)).toBe('/projects');
			expect((await sessionSet(res as Response))?.get('currentProjectID')).toBeUndefined();
		});
	});
});
