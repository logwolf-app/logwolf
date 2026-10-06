import { data, Outlet, redirect } from 'react-router';

import { AppSidebar } from '~/components/nav/app-sidebar';
import { SidebarInset, SidebarProvider } from '~/components/ui/sidebar';
import { Toaster } from '~/components/ui/sonner';
import { eventContext } from '~/context';
import { createApi } from '~/lib/api';
import { requireAuth } from '~/lib/auth.server';
import { getOrCreateCsrfToken } from '~/lib/csrf.server';
import { resolveCurrent } from '~/lib/organizations';
import { commitSession, getSession } from '~/lib/session.server';
import { ThemeProvider } from '~/store/theme-provider';

import type { Route } from './+types/layout';

export async function loader({ request, context }: Route.LoaderArgs) {
	const event = context.get(eventContext);
	event?.addTag('loader');

	const user = await requireAuth(request);

	const session = await getSession(request.headers.get('Cookie'));
	const csrfToken = getOrCreateCsrfToken(session);

	const api = createApi(user);
	const [projects, organizations] = await Promise.all([api.getProjects(), api.getOrganizations()]);
	const url = new URL(request.url);

	// The stored organization and project are only usable while the user can
	// still reach them — a deleted one, or one the user was removed from, falls
	// back to what they can (resolveCurrent). The project must also be in view
	// of the organization: its own, or one shared from an organization the user
	// is not in.
	const storedProjectID = session.get('currentProjectID');
	const storedOrganizationID = session.get('currentOrganizationID');
	const {
		organization: currentOrganization,
		project: currentProject,
		inView,
	} = resolveCurrent({ projects, organizations, storedProjectID, storedOrganizationID });

	if (currentProject?.id !== storedProjectID || currentOrganization?.id !== storedOrganizationID) {
		if (currentProject) session.set('currentProjectID', currentProject.id);
		else session.unset('currentProjectID');
		if (currentOrganization) session.set('currentOrganizationID', currentOrganization.id);
		else session.unset('currentOrganizationID');

		// Child loaders of this request already read the stale cookie, so reload
		// the same URL to let them run against the corrected session.
		throw redirect(`${url.pathname}${url.search}`, {
			headers: { 'Set-Cookie': await commitSession(session) },
		});
	}

	// Every page under this layout is scoped to a project, so a user who has
	// none in view has nothing to render. Creating one is the way forward (or
	// switching organization, which the sidebar there offers), and that page is
	// the one place reachable without a project in session.
	if (inView.length === 0 && url.pathname !== '/projects/new') {
		throw redirect('/projects/new');
	}

	event?.set('currentProjectID', currentProject?.id ?? null);
	event?.set('currentOrganizationID', currentOrganization?.id ?? null);

	return data(
		{ user, csrfToken, projects, currentProject, organizations, currentOrganization },
		{ headers: { 'Set-Cookie': await commitSession(session) } },
	);
}

export default function Layout({ matches, loaderData }: Route.ComponentProps) {
	const { user, projects, currentProject, organizations, currentOrganization, csrfToken } = loaderData;

	return (
		<ThemeProvider>
			<SidebarProvider>
				<AppSidebar
					matches={matches}
					projects={projects}
					currentProject={currentProject}
					organizations={organizations}
					currentOrganization={currentOrganization}
					csrfToken={csrfToken}
					user={user}
				/>

				<SidebarInset>
					<Outlet />
					<Toaster />
				</SidebarInset>
			</SidebarProvider>
		</ThemeProvider>
	);
}
