import type { OrganizationPlan, OrganizationRole, Project } from './api';

type HasId = { id: string };

/**
 * The projects the dashboard offers while the user works in `organization`:
 * the organization's own, and those `shared` with the user project by project
 * from organizations they are not a member of, which no organization switch
 * would ever reach. Projects of the user's other organizations wait for a
 * switch to those. With no organization, the user is a member of none, and
 * every project is their own.
 */
export function projectGroups<P extends Project>(
	projects: P[],
	organizations: HasId[],
	organization?: HasId,
): { own: P[]; shared: P[] } {
	if (!organization) return { own: projects, shared: [] };

	return {
		own: projects.filter((p) => p.organization_id === organization.id),
		shared: projects.filter((p) => !organizations.some((o) => o.id === p.organization_id)),
	};
}

/**
 * Resolves what the session stored against what the user can still reach. The
 * organization is the stored one while the user is a member of it; otherwise
 * the stored project's, then their first. The project is the stored one while
 * it is in view (`projectGroups`); otherwise the first in view, the
 * organization's own before the shared ones. Either is undefined when there is
 * nothing to pick.
 */
export function resolveCurrent<P extends Project, O extends HasId>({
	projects,
	organizations,
	storedProjectID,
	storedOrganizationID,
}: {
	projects: P[];
	organizations: O[];
	storedProjectID: string | undefined;
	storedOrganizationID: string | undefined;
}): { organization: O | undefined; project: P | undefined; inView: P[] } {
	const storedProject = projects.find((p) => p.id === storedProjectID);
	const organization =
		organizations.find((o) => o.id === storedOrganizationID) ??
		organizations.find((o) => o.id === storedProject?.organization_id) ??
		organizations.at(0);

	const { own, shared } = projectGroups(projects, organizations, organization);
	const inView = [...own, ...shared];
	const project = inView.find((p) => p.id === storedProjectID) ?? inView.at(0);

	return { organization, project, inView };
}

/** Pages that read nothing but the session, so they make sense in any organization. */
const ORGANIZATION_NEUTRAL_PAGES = new Set(['/dashboard', '/events', '/keys', '/projects', '/projects/new']);

/**
 * Where to land after switching to `organizationId` from `pathname`: the same
 * page when it follows the session, the new organization's settings from
 * another's, and the dashboard from anything tied to a project of the old one.
 */
export function pathAfterOrganizationSwitch(pathname: string, organizationId: string): string {
	if (/^\/organizations\/[^/]+\/settings\/?$/.test(pathname)) return `/organizations/${organizationId}/settings`;
	return ORGANIZATION_NEUTRAL_PAGES.has(pathname) ? pathname : '/dashboard';
}

/** Whether the role may rename the organization and manage its members. */
export const canManageOrganization = (role: OrganizationRole) => role === 'owner' || role === 'admin';

/** The roles `role` may hand out: only an owner makes owners. */
export function assignableOrganizationRoles(role: OrganizationRole): OrganizationRole[] {
	return role === 'owner' ? ['member', 'admin', 'owner'] : ['member', 'admin'];
}

/** The role as a sentence names it: "an owner", "an admin", "a member". */
export const withArticle = (role: OrganizationRole) => (role === 'member' ? 'a member' : `an ${role}`);

/**
 * The plan of every organization on a self-hosted install
 * (`data.SelfHostedPlan`), which limits nothing.
 */
export const SELF_HOSTED_PLAN = 'selfhosted';

/**
 * Whether the organization has used its plan's monthly events: the broker
 * refuses its projects' events, the dashboard's too, until the month is over.
 * A plan without a monthly limit never has.
 */
export function overQuota({ plan, usage }: OrganizationPlan): boolean {
	return plan.monthly_events > 0 && usage.events >= plan.monthly_events;
}

/** When a quota used up at `now` renews: the first instant of the next month, in UTC. */
export function quotaRenewsAt(now: Date = new Date()): Date {
	return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1));
}

/** The day a quota renews, as the dashboard words it: "November 1". */
export function formatRenewal(renewsAt: Date | string, locale: string): string {
	return new Date(renewsAt).toLocaleDateString(locale, { month: 'long', day: 'numeric', timeZone: 'UTC' });
}
