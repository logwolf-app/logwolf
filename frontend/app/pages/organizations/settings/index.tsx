import { redirect } from 'react-router';

import { Page } from '~/components/nav/page';
import { eventContext } from '~/context';
import { allowlistFromEnv, checkInvitee, inviteeFromCheck, inviteWarning } from '~/lib/allowlist.server';
import { createApi, type OrganizationRole } from '~/lib/api';
import { requireAuth } from '~/lib/auth.server';
import { validateCsrfToken } from '~/lib/csrf.server';
import { canManageOrganization, withArticle } from '~/lib/organizations';
import { getGithubToken } from '~/lib/session.server';

import type { Route } from './+types';
import { GeneralSection } from './components/general-section';
import { MembersSection } from './components/members-section';
import { PlanSection } from './components/plan-section';

export function meta({ data }: Route.MetaArgs) {
	return [{ title: `${data?.organization.name ?? 'Organization'} settings - Logwolf` }];
}

const ROLES: readonly OrganizationRole[] = ['member', 'admin', 'owner'];

function parseRole(value: FormDataEntryValue | null): OrganizationRole | undefined {
	return ROLES.find((r) => r === value?.toString());
}

/**
 * Resolves the organization in the URL against the organizations the caller is
 * a member of, like the project settings page does with projects: one list
 * call says both whether they may open the page and what role they hold, where
 * the broker would answer a non-member with a bare 403.
 */
async function requireOrganization(request: Request, id: string | undefined) {
	const user = await requireAuth(request);
	const api = createApi(user);

	const organizations = await api.getOrganizations();
	const organization = organizations.find((o) => o.id === id);
	if (!organization) throw redirect('/dashboard');

	return { user, api, organization };
}

export async function loader({ request, params, context }: Route.LoaderArgs) {
	const event = context.get(eventContext);
	event?.addTag('loader');

	const { user, api, organization } = await requireOrganization(request, params.id);

	const [members, plan] = await Promise.all([
		api.getOrganizationMembers(organization.id),
		api.getOrganizationPlan(organization.id),
	]);
	event?.set('loaderData', { organization, memberCount: members.length, plan: plan.plan.name });

	return { organization, members, plan, currentUser: { id: user.id, login: user.login } };
}

export async function action({ request, params, context }: Route.ActionArgs) {
	const event = context.get(eventContext);
	event?.addTag('action');

	const { api, organization } = await requireOrganization(request, params.id);

	const fd = await request.formData();
	await validateCsrfToken(request, fd);

	const intent = fd.get('intent')?.toString() ?? '';
	event?.set('intent', intent);

	// Owners and admins manage the organization, and only owners touch owners.
	// The broker enforces both; repeating what the form alone shows turns a bare
	// "forbidden" from a stale tab into a message next to the control. Whether a
	// change touches an existing owner only the broker knows, and its refusal
	// says so in words the page can show.
	if (!canManageOrganization(organization.role)) {
		return { error: 'Only an owner or an admin can change this.' };
	}

	try {
		if (intent === 'rename') {
			const name = fd.get('name')?.toString().trim() ?? '';
			if (!name) return { error: 'Name is required.' };

			await api.updateOrganization(organization.id, name);
			return { success: `Renamed to ${name}.` };
		}

		if (intent === 'add-member') {
			const login = fd.get('login')?.toString().trim() ?? '';
			const role = parseRole(fd.get('role'));
			if (!login) return { error: 'A GitHub login is required.' };
			if (!role) return { error: 'Choose member, admin or owner.' };
			if (role === 'owner' && organization.role !== 'owner') return { error: 'Only an owner can add an owner.' };

			// The same check as a project invite, asked as the inviting user.
			const check = await checkInvitee(login, allowlistFromEnv(), fetch, await getGithubToken(request));
			event?.set('inviteCheck', check.kind);
			const resolved = inviteeFromCheck(login, check);
			if ('error' in resolved) return resolved;

			await api.addOrganizationMember(organization.id, resolved.invitee, role);
			return { success: `Added ${resolved.invitee.login} as ${role}.`, warning: inviteWarning(check) };
		}

		// Members are named by their membership's id; the login only words the
		// message.
		if (intent === 'change-role') {
			const member = fd.get('member')?.toString() ?? '';
			const login = fd.get('login')?.toString() ?? '';
			const role = parseRole(fd.get('role'));
			if (!role) return { error: 'Choose member, admin or owner.' };
			if (role === 'owner' && organization.role !== 'owner') {
				return { error: 'Only an owner can make someone an owner.' };
			}

			await api.updateOrganizationMemberRole(organization.id, member, role);
			return { success: `${login} is now ${withArticle(role)}.` };
		}

		if (intent === 'remove-member') {
			const member = fd.get('member')?.toString() ?? '';
			const login = fd.get('login')?.toString() ?? '';
			await api.removeOrganizationMember(organization.id, member);
			return { success: `Removed ${login}.` };
		}

		return null;
	} catch (err) {
		event?.setSeverity('error');
		event?.set('actionError', err);
		return { error: (err as Error).message };
	}
}

export default function OrganizationSettings({ loaderData }: Route.ComponentProps) {
	const { organization, members, plan, currentUser } = loaderData;
	const canManage = canManageOrganization(organization.role);

	return (
		<Page
			title='Organization settings'
			description={
				<>
					Settings for <span className='font-medium text-foreground'>{organization.name}</span>. You are{' '}
					{withArticle(organization.role)} of this organization.
				</>
			}
		>
			<div className='flex flex-col gap-8'>
				<GeneralSection organization={organization} canEdit={canManage} />
				<PlanSection plan={plan} />
				<MembersSection members={members} currentUser={currentUser} callerRole={organization.role} />
			</div>
		</Page>
	);
}
