import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createApi } from '~/lib/api';
import {
	context,
	fakeApi,
	get,
	location,
	organization,
	project,
	sessionCookie,
	sessionSet,
	thrown,
} from '~/test/routes';

import { loader } from './layout';

vi.mock('~/lib/api', async (importOriginal) => ({
	...(await importOriginal<typeof import('~/lib/api')>()),
	createApi: vi.fn(),
}));

const alpha = 'aaaaaaaaaaaaaaaaaaaaaaa1';
const beta = 'bbbbbbbbbbbbbbbbbbbbbbb2';
const gamma = 'ccccccccccccccccccccccc3';
const acme = 'ddddddddddddddddddddddd4';
const globex = 'eeeeeeeeeeeeeeeeeeeeeee5';

type LayoutData = {
	data: {
		currentProject?: { id: string };
		currentOrganization?: { id: string };
		projects: unknown[];
		organizations: unknown[];
		csrfToken: string;
	};
};

function load(path: string, cookie: string) {
	return loader({ request: get(path, cookie), params: {}, context } as never);
}

describe('layout loader', () => {
	let projects: ReturnType<typeof project>[];
	let organizations: ReturnType<typeof organization>[];

	beforeEach(() => {
		projects = [project(alpha), project(beta, 'member')];
		organizations = [];
		vi.mocked(createApi).mockReturnValue(
			fakeApi({ getProjects: async () => projects, getOrganizations: async () => organizations }),
		);
	});

	it('renders with the stored project when the user still belongs to it', async () => {
		const res = (await load('/events', await sessionCookie({ currentProjectID: beta }))) as LayoutData;

		expect(res.data.currentProject?.id).toBe(beta);
		expect(res.data.projects).toHaveLength(2);
		expect(res.data.csrfToken).toBeTruthy();
	});

	// Deleted, or the user was removed from it: the child loaders of this request
	// already read the stale id, so the layout reloads the same URL with a fixed one.
	it('re-points a stale project at the first one left, and reloads the same URL', async () => {
		const res = await thrown(
			load('/events?page=3', await sessionCookie({ currentProjectID: 'fffffffffffffffffffffff6' })),
		);

		expect(location(res)).toBe('/events?page=3');
		expect((await sessionSet(res as Response))?.get('currentProjectID')).toBe(alpha);
	});

	it('picks the first project for a session that has none yet', async () => {
		const res = await thrown(load('/dashboard', await sessionCookie()));

		expect(location(res)).toBe('/dashboard');
		expect((await sessionSet(res as Response))?.get('currentProjectID')).toBe(alpha);
	});

	it('sends a user with no projects to create one, clearing the stale id on the way', async () => {
		projects = [];

		const first = await thrown(load('/events', await sessionCookie({ currentProjectID: alpha })));
		expect(location(first)).toBe('/events');
		const cleared = await sessionSet(first as Response);
		expect(cleared?.get('currentProjectID')).toBeUndefined();

		const second = await thrown(load('/events', await sessionCookie()));
		expect(location(second)).toBe('/projects/new');
	});

	it('renders /projects/new for a user with no projects, instead of looping', async () => {
		projects = [];

		const res = (await load('/projects/new', await sessionCookie())) as LayoutData;
		expect(res.data.currentProject).toBeUndefined();
	});

	it('sends a signed-out user to sign in, without asking the broker', async () => {
		const err = await thrown(load('/dashboard', await sessionCookie({ signedIn: false })));

		expect(location(err)).toBe('/auth');
		expect(createApi).not.toHaveBeenCalled();
	});

	describe('organizations', () => {
		beforeEach(() => {
			organizations = [organization(acme), organization(globex, 'member')];
			projects = [project(alpha, 'owner', 'Alpha', acme), project(beta, 'owner', 'Beta', globex)];
		});

		it('renders with the stored organization and a project of it', async () => {
			const res = (await load(
				'/dashboard',
				await sessionCookie({ currentProjectID: beta, currentOrganizationID: globex }),
			)) as LayoutData;

			expect(res.data.currentOrganization?.id).toBe(globex);
			expect(res.data.currentProject?.id).toBe(beta);
			expect(res.data.organizations).toHaveLength(2);
		});

		it('picks the stored project’s organization for a session that has none yet', async () => {
			const res = await thrown(load('/dashboard', await sessionCookie({ currentProjectID: beta })));

			expect(location(res)).toBe('/dashboard');
			const session = await sessionSet(res as Response);
			expect(session?.get('currentOrganizationID')).toBe(globex);
			expect(session?.get('currentProjectID')).toBe(beta);
		});

		it('re-points an organization the user left at their first, and its first project', async () => {
			const res = await thrown(
				load(
					'/events',
					await sessionCookie({ currentProjectID: 'fffffffffffffffffffffff6', currentOrganizationID: gamma }),
				),
			);

			expect(location(res)).toBe('/events');
			const session = await sessionSet(res as Response);
			expect(session?.get('currentOrganizationID')).toBe(acme);
			expect(session?.get('currentProjectID')).toBe(alpha);
		});

		it('moves off a project of another of the user’s organizations', async () => {
			const res = await thrown(
				load('/dashboard', await sessionCookie({ currentProjectID: beta, currentOrganizationID: acme })),
			);

			expect((await sessionSet(res as Response))?.get('currentProjectID')).toBe(alpha);
		});

		it('keeps a project shared from an organization the user is not in', async () => {
			projects.push(project(gamma, 'member', 'Shared', 'fffffffffffffffffffffff6'));

			const res = (await load(
				'/dashboard',
				await sessionCookie({ currentProjectID: gamma, currentOrganizationID: acme }),
			)) as LayoutData;
			expect(res.data.currentProject?.id).toBe(gamma);
		});

		it('sends a user whose organization has no projects to create one there', async () => {
			projects = [project(beta, 'owner', 'Beta', globex)];

			const first = await thrown(
				load('/events', await sessionCookie({ currentProjectID: beta, currentOrganizationID: acme })),
			);
			const cleared = await sessionSet(first as Response);
			expect(cleared?.get('currentProjectID')).toBeUndefined();
			expect(cleared?.get('currentOrganizationID')).toBe(acme);

			const second = await thrown(load('/events', await sessionCookie({ currentOrganizationID: acme })));
			expect(location(second)).toBe('/projects/new');

			const res = (await load('/projects/new', await sessionCookie({ currentOrganizationID: acme }))) as LayoutData;
			expect(res.data.currentOrganization?.id).toBe(acme);
			expect(res.data.currentProject).toBeUndefined();
		});
	});
});
