import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createApi } from '~/lib/api';
import {
	context,
	fakeApi,
	location,
	organization,
	post,
	project,
	sessionCookie,
	sessionSet,
	thrown,
} from '~/test/routes';

import { action } from './index';

vi.mock('~/lib/api', async (importOriginal) => ({
	...(await importOriginal<typeof import('~/lib/api')>()),
	createApi: vi.fn(),
}));

const acme = 'ddddddddddddddddddddddd4';
const globex = 'eeeeeeeeeeeeeeeeeeeeeee5';
const initech = 'fffffffffffffffffffffff6';
const alpha = 'aaaaaaaaaaaaaaaaaaaaaaa1';
const beta = 'bbbbbbbbbbbbbbbbbbbbbbb2';
const shared = 'ccccccccccccccccccccccc3';

describe('/organizations/switch action', () => {
	let api: ReturnType<typeof fakeApi>;

	beforeEach(() => {
		api = fakeApi({
			getOrganizations: async () => [organization(acme), organization(globex, 'member'), organization(initech)],
			getProjects: async () => [
				project(alpha, 'owner', 'Alpha', acme),
				project(beta, 'owner', 'Beta', globex),
				project(shared, 'member', 'Shared', '999999999999999999999999'),
			],
		});
		vi.mocked(createApi).mockReturnValue(api);
	});

	async function switchTo(
		organizationId: string,
		{
			redirectTo = '/events',
			currentProjectID = alpha,
			csrf,
		}: { redirectTo?: string; currentProjectID?: string; csrf?: string | null } = {},
	) {
		const cookie = await sessionCookie({ currentProjectID, currentOrganizationID: acme });
		return action({
			request: post('/organizations/switch', { organizationId, redirectTo }, cookie, { csrf }),
			params: {},
			context,
		} as never);
	}

	it('points the session at an organization the user is in, and at its first project', async () => {
		const res = await switchTo(globex, { redirectTo: '/events?page=2' });

		expect(location(res)).toBe('/events?page=2');
		const session = await sessionSet(res as Response);
		expect(session?.get('currentOrganizationID')).toBe(globex);
		expect(session?.get('currentProjectID')).toBe(beta);
	});

	it('keeps the project when it is already in the organization', async () => {
		const res = await switchTo(acme, { currentProjectID: alpha });

		const session = await sessionSet(res as Response);
		expect(session?.get('currentProjectID')).toBe(alpha);
	});

	// Then the layout decides: a project shared with the user, or /projects/new.
	it('clears the project when the organization has none', async () => {
		const session = await sessionSet((await switchTo(initech)) as Response);

		expect(session?.get('currentOrganizationID')).toBe(initech);
		expect(session?.get('currentProjectID')).toBeUndefined();
	});

	it('moves off a shared project to the organization’s own', async () => {
		const session = await sessionSet((await switchTo(globex, { currentProjectID: shared })) as Response);

		expect(session?.get('currentProjectID')).toBe(beta);
	});

	it('leaves the session alone for an organization the user is not in', async () => {
		const res = await switchTo('999999999999999999999999');

		expect(location(res)).toBe('/events');
		expect(await sessionSet(res as Response)).toBeNull();
	});

	it.each(['https://evil.example/', '//evil.example/', '/\\evil.example/', 'events'])(
		'does not redirect off the dashboard to %s',
		async (redirectTo) => {
			expect(location(await switchTo(globex, { redirectTo }))).toBe('/dashboard');
		},
	);

	it('refuses a form without the session’s CSRF token', async () => {
		for (const csrf of [null, 'forged']) {
			const err = await thrown(switchTo(globex, { csrf }));
			expect(err).toMatchObject({ init: { status: 403 } });
		}
		expect(api.getOrganizations).not.toHaveBeenCalled();
	});

	it('sends a signed-out user to sign in', async () => {
		const cookie = await sessionCookie({ signedIn: false });
		const err = await thrown(
			action({
				request: post('/organizations/switch', { organizationId: globex }, cookie),
				params: {},
				context,
			} as never),
		);
		expect(location(err)).toBe('/auth');
	});
});
