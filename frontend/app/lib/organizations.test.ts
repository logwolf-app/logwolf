import { describe, expect, it } from 'vitest';

import type { UserProject } from './api';
import {
	assignableOrganizationRoles,
	canManageOrganization,
	pathAfterOrganizationSwitch,
	projectGroups,
	resolveCurrent,
} from './organizations';

const acme = { id: 'aaaaaaaaaaaaaaaaaaaaaaa1' };
const globex = { id: 'bbbbbbbbbbbbbbbbbbbbbbb2' };
const elsewhere = 'ccccccccccccccccccccccc3';

function project(id: string, organization_id?: string): UserProject {
	return { id, name: id, slug: id, organization_id, created_at: '2026-01-01T00:00:00Z', role: 'owner' };
}

const inAcme = project('in-acme', acme.id);
const alsoInAcme = project('also-in-acme', acme.id);
const inGlobex = project('in-globex', globex.id);
const shared = project('shared', elsewhere);
const projects = [inGlobex, shared, inAcme, alsoInAcme];

describe('projectGroups', () => {
	it('splits the organization’s own projects from those shared from organizations the user is not in', () => {
		expect(projectGroups(projects, [acme, globex], acme)).toEqual({ own: [inAcme, alsoInAcme], shared: [shared] });
		expect(projectGroups(projects, [acme, globex], globex)).toEqual({ own: [inGlobex], shared: [shared] });
	});

	it('counts every project as the user’s own when they are in no organization', () => {
		expect(projectGroups(projects, [])).toEqual({ own: projects, shared: [] });
	});
});

describe('resolveCurrent', () => {
	const resolve = (storedProjectID?: string, storedOrganizationID?: string, organizations = [acme, globex]) =>
		resolveCurrent({ projects, organizations, storedProjectID, storedOrganizationID });

	it('keeps what the session stored while it is still in reach', () => {
		const { organization, project } = resolve(alsoInAcme.id, acme.id);
		expect(organization).toBe(acme);
		expect(project).toBe(alsoInAcme);
	});

	it('keeps a shared project in any organization', () => {
		expect(resolve(shared.id, globex.id).project).toBe(shared);
	});

	it('follows the stored project to its organization when the stored one is gone', () => {
		expect(resolve(inGlobex.id, elsewhere).organization).toBe(globex);
		expect(resolve(inGlobex.id).organization).toBe(globex);
	});

	it('falls back to the first organization, and to its first project before a shared one', () => {
		const { organization, project } = resolve();
		expect(organization).toBe(acme);
		expect(project).toBe(inAcme);
	});

	it('moves off a project of another of the user’s organizations', () => {
		expect(resolve(inGlobex.id, acme.id).project).toBe(inAcme);
	});

	it('offers a shared project when the organization has none of its own', () => {
		const { project, inView } = resolveCurrent({
			projects: [shared, inGlobex],
			organizations: [acme, globex],
			storedProjectID: undefined,
			storedOrganizationID: acme.id,
		});
		expect(project).toBe(shared);
		expect(inView).toEqual([shared]);
	});

	it('picks nothing when nothing is in view', () => {
		const { project, inView } = resolveCurrent({
			projects: [inGlobex],
			organizations: [acme, globex],
			storedProjectID: inGlobex.id,
			storedOrganizationID: acme.id,
		});
		expect(project).toBeUndefined();
		expect(inView).toEqual([]);
	});

	it('works as before organizations for a user who is in none', () => {
		const { organization, project } = resolve(inGlobex.id, acme.id, []);
		expect(organization).toBeUndefined();
		expect(project).toBe(inGlobex);
	});
});

describe('pathAfterOrganizationSwitch', () => {
	it.each([
		['/dashboard', '/dashboard'],
		['/events', '/events'],
		['/keys', '/keys'],
		['/projects/new', '/projects/new'],
		[`/organizations/${acme.id}/settings`, `/organizations/${globex.id}/settings`],
		[`/projects/${inAcme.id}/settings`, '/dashboard'],
		['/events/some-event', '/dashboard'],
	])('from %s lands on %s', (from, to) => {
		expect(pathAfterOrganizationSwitch(from, globex.id)).toBe(to);
	});
});

describe('organization roles', () => {
	it('lets owners and admins manage the organization', () => {
		expect(canManageOrganization('owner')).toBe(true);
		expect(canManageOrganization('admin')).toBe(true);
		expect(canManageOrganization('member')).toBe(false);
	});

	it('lets only an owner make owners', () => {
		expect(assignableOrganizationRoles('owner')).toContain('owner');
		expect(assignableOrganizationRoles('admin')).not.toContain('owner');
	});
});
