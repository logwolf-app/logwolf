import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createApi } from '~/lib/api';
import { context, fakeApi, location, post, project, sessionCookie, sessionSet, thrown } from '~/test/routes';

import { action } from './index';

vi.mock('~/lib/api', async (importOriginal) => ({
	...(await importOriginal<typeof import('~/lib/api')>()),
	createApi: vi.fn(),
}));

const acme = 'ddddddddddddddddddddddd4';
const created = 'aaaaaaaaaaaaaaaaaaaaaaa1';

describe('/projects/new action', () => {
	let api: ReturnType<typeof fakeApi>;

	beforeEach(() => {
		api = fakeApi({ createProject: async () => project(created, 'owner', 'My App', acme) });
		vi.mocked(createApi).mockReturnValue(api);
	});

	function create(fields: Record<string, string>, cookie: string, opts?: { csrf?: string | null }) {
		return action({ request: post('/projects/new', fields, cookie, opts), params: {}, context } as never);
	}

	it('creates the project in the organization in session, and opens it', async () => {
		const res = await create({ name: ' My App ' }, await sessionCookie({ currentOrganizationID: acme }));

		expect(api.createProject).toHaveBeenCalledWith('My App', 'my-app', acme);
		expect(location(res)).toBe('/dashboard');
		expect((await sessionSet(res as Response))?.get('currentProjectID')).toBe(created);
	});

	// The organization is the session's: one named by the form is not even read.
	it('ignores an organization the form names', async () => {
		await create(
			{ name: 'My App', organizationId: '999999999999999999999999' },
			await sessionCookie({ currentOrganizationID: acme }),
		);

		expect(api.createProject).toHaveBeenCalledWith('My App', 'my-app', acme);
	});

	it('creates it where projects went before organizations for a user in none', async () => {
		await create({ name: 'My App' }, await sessionCookie());

		expect(api.createProject).toHaveBeenCalledWith('My App', 'my-app', undefined);
	});

	it('shows the broker’s refusal', async () => {
		api.createProject.mockRejectedValue(new Error('forbidden'));

		expect(await create({ name: 'My App' }, await sessionCookie({ currentOrganizationID: acme }))).toEqual({
			error: 'forbidden',
		});
	});

	it('needs a name with a letter or number in it', async () => {
		const cookie = await sessionCookie({ currentOrganizationID: acme });

		expect(await create({ name: ' ' }, cookie)).toEqual({ error: 'Name is required.' });
		expect(await create({ name: '!!!' }, cookie)).toEqual({
			error: 'Name must contain at least one letter or number.',
		});
		expect(api.createProject).not.toHaveBeenCalled();
	});

	it('refuses a form without the session’s CSRF token', async () => {
		const err = await thrown(create({ name: 'My App' }, await sessionCookie(), { csrf: 'forged' }));

		expect(err).toMatchObject({ init: { status: 403 } });
		expect(api.createProject).not.toHaveBeenCalled();
	});
});
