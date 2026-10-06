import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createApi } from '~/lib/api';
import { context, fakeApi, get, location, organization, post, sessionCookie, thrown, user } from '~/test/routes';

import { action, loader } from './index';

vi.mock('~/lib/api', async (importOriginal) => ({
	...(await importOriginal<typeof import('~/lib/api')>()),
	createApi: vi.fn(),
}));

const owned = 'ddddddddddddddddddddddd4';
const administered = 'eeeeeeeeeeeeeeeeeeeeeee5';
const joined = 'fffffffffffffffffffffff6';
const outside = '999999999999999999999999';
const member = 'aaaaaaaaaaaaaaaaaaaaaaa1';

function send(id: string, fields: Record<string, string>, cookie: string, opts?: { csrf?: string | null }) {
	return action({
		request: post(`/organizations/${id}/settings`, fields, cookie, opts),
		params: { id },
		context,
	} as never);
}

describe('/organizations/:id/settings', () => {
	let api: ReturnType<typeof fakeApi>;
	let cookie: string;

	beforeEach(async () => {
		api = fakeApi({
			getOrganizations: async () => [
				organization(owned, 'owner', 'Owned'),
				organization(administered, 'admin', 'Administered'),
				organization(joined, 'member', 'Joined'),
			],
			updateOrganization: async () => organization(owned),
			addOrganizationMember: async () => {},
			updateOrganizationMemberRole: async () => {},
			removeOrganizationMember: async () => {},
		});
		vi.mocked(createApi).mockReturnValue(api);
		cookie = await sessionCookie({ currentOrganizationID: owned });
	});

	afterEach(() => {
		vi.unstubAllGlobals();
		vi.unstubAllEnvs();
	});

	it('loads the organization with the caller’s role, its members and its plan', async () => {
		const plan = {
			plan: { name: 'selfhosted', monthly_events: 0, max_retention_days: 0, max_projects: 0, max_members: 0 },
			usage: { projects: 2, members: 3, events: 1234 },
		};
		api.getOrganizationMembers.mockResolvedValue([]);
		api.getOrganizationPlan.mockResolvedValue(plan);

		const data = await loader({
			request: get(`/organizations/${administered}/settings`, cookie),
			params: { id: administered },
			context,
		} as never);

		expect(data).toMatchObject({
			organization: { id: administered, role: 'admin' },
			members: [],
			plan,
			currentUser: { id: user.id, login: user.login },
		});
		expect(api.getOrganizationMembers).toHaveBeenCalledWith(administered);
		expect(api.getOrganizationPlan).toHaveBeenCalledWith(administered);
	});

	it('sends an organization the user is not in back to the dashboard', async () => {
		const err = await thrown(
			loader({ request: get(`/organizations/${outside}/settings`, cookie), params: { id: outside }, context } as never),
		);
		expect(location(err)).toBe('/dashboard');

		expect(location(await thrown(send(outside, { intent: 'rename', name: 'Mine now' }, cookie)))).toBe('/dashboard');
		expect(api.updateOrganization).not.toHaveBeenCalled();
	});

	it('refuses a form without the session’s CSRF token', async () => {
		const err = await thrown(send(owned, { intent: 'rename', name: 'X' }, cookie, { csrf: 'forged' }));
		expect(err).toMatchObject({ init: { status: 403 } });
		expect(api.updateOrganization).not.toHaveBeenCalled();
	});

	it.each<Record<string, string>>([
		{ intent: 'rename', name: 'Renamed' },
		{ intent: 'add-member', login: 'someone', role: 'member' },
		{ intent: 'change-role', member, login: 'someone', role: 'admin' },
		{ intent: 'remove-member', member, login: 'someone' },
	])('lets only an owner or an admin $intent', async (fields) => {
		expect(await send(joined, fields, cookie)).toEqual({ error: 'Only an owner or an admin can change this.' });
		for (const m of [
			'updateOrganization',
			'addOrganizationMember',
			'updateOrganizationMemberRole',
			'removeOrganizationMember',
		] as const) {
			expect(api[m]).not.toHaveBeenCalled();
		}
	});

	it('renames', async () => {
		expect(await send(administered, { intent: 'rename', name: '  Renamed  ' }, cookie)).toEqual({
			success: 'Renamed to Renamed.',
		});
		expect(api.updateOrganization).toHaveBeenCalledWith(administered, 'Renamed');

		expect(await send(administered, { intent: 'rename', name: ' ' }, cookie)).toEqual({ error: 'Name is required.' });
	});

	describe('add-member', () => {
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

		it('adds a GitHub user by ID, under GitHub’s casing', async () => {
			stubGitHub();
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_USERS', 'octodog');

			const res = await send(administered, { intent: 'add-member', login: 'octodog', role: 'admin' }, cookie);

			expect(api.addOrganizationMember).toHaveBeenCalledWith(administered, { id: 9001, login: 'OctoDog' }, 'admin');
			expect(res).toEqual({ success: 'Added OctoDog as admin.', warning: undefined });
		});

		it('warns about someone the allowlist does not clear', async () => {
			stubGitHub();
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_USERS', 'octocat');
			vi.stubEnv('LOGWOLF_ALLOWED_GITHUB_ORGS', '');

			const res = await send(owned, { intent: 'add-member', login: 'octodog', role: 'member' }, cookie);
			expect(res).toMatchObject({ warning: expect.stringMatching(/cannot sign in/) });
		});

		it('refuses a login GitHub does not know, or an organization', async () => {
			stubGitHub();

			expect(await send(owned, { intent: 'add-member', login: 'nobody', role: 'member' }, cookie)).toEqual({
				error: 'There is no GitHub user named nobody.',
			});
			expect(await send(owned, { intent: 'add-member', login: 'acme', role: 'member' }, cookie)).toMatchObject({
				error: expect.stringMatching(/organization/),
			});
			expect(api.addOrganizationMember).not.toHaveBeenCalled();
		});

		it('lets only an owner add an owner, before asking GitHub', async () => {
			const fetchMock = vi.fn();
			vi.stubGlobal('fetch', fetchMock);

			expect(await send(administered, { intent: 'add-member', login: 'octodog', role: 'owner' }, cookie)).toEqual({
				error: 'Only an owner can add an owner.',
			});
			expect(fetchMock).not.toHaveBeenCalled();
			expect(api.addOrganizationMember).not.toHaveBeenCalled();
		});

		it('refuses a role organizations do not have', async () => {
			expect(await send(owned, { intent: 'add-member', login: 'octodog', role: 'superuser' }, cookie)).toEqual({
				error: 'Choose member, admin or owner.',
			});
		});
	});

	describe('members', () => {
		it('changes a role by the membership’s id', async () => {
			const res = await send(administered, { intent: 'change-role', member, login: 'someone', role: 'admin' }, cookie);

			expect(api.updateOrganizationMemberRole).toHaveBeenCalledWith(administered, member, 'admin');
			expect(res).toEqual({ success: 'someone is now an admin.' });
		});

		it('lets only an owner make an owner', async () => {
			expect(
				await send(administered, { intent: 'change-role', member, login: 'someone', role: 'owner' }, cookie),
			).toEqual({ error: 'Only an owner can make someone an owner.' });
			expect(api.updateOrganizationMemberRole).not.toHaveBeenCalled();

			await send(owned, { intent: 'change-role', member, login: 'someone', role: 'owner' }, cookie);
			expect(api.updateOrganizationMemberRole).toHaveBeenCalledWith(owned, member, 'owner');
		});

		it('removes by the membership’s id', async () => {
			const res = await send(administered, { intent: 'remove-member', member, login: 'someone' }, cookie);

			expect(api.removeOrganizationMember).toHaveBeenCalledWith(administered, member);
			expect(res).toEqual({ success: 'Removed someone.' });
		});

		// Whether a member is an owner only the broker knows for sure; its
		// refusal is what the page shows.
		it('shows the broker’s refusal', async () => {
			api.removeOrganizationMember.mockRejectedValue(new Error('only an owner can remove an owner'));

			expect(await send(administered, { intent: 'remove-member', member, login: 'boss' }, cookie)).toEqual({
				error: 'only an owner can remove an owner',
			});
		});
	});
});
