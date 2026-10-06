import { redirect } from 'react-router';

import { eventContext } from '~/context';
import { createApi } from '~/lib/api';
import { requireAuth } from '~/lib/auth.server';
import { validateCsrfToken } from '~/lib/csrf.server';
import { safeRedirect } from '~/lib/redirect';
import { commitSession, getSession } from '~/lib/session.server';

import type { Route } from './+types';

const FALLBACK_REDIRECT = '/dashboard';

export async function action({ request, context }: Route.ActionArgs) {
	const event = context.get(eventContext);
	event?.addTag('action');

	const user = await requireAuth(request);
	const fd = await request.formData();

	await validateCsrfToken(request, fd);

	const organizationId = fd.get('organizationId')?.toString() ?? '';
	const redirectTo = safeRedirect(fd.get('redirectTo')?.toString(), FALLBACK_REDIRECT);
	event?.set('organizationId', organizationId);

	// The id comes from the browser, so membership is re-checked here, as the
	// project switcher does; an organization the user is not in leaves the
	// session alone.
	const api = createApi(user);
	const [organizations, projects] = await Promise.all([api.getOrganizations(), api.getProjects()]);

	if (!organizations.some((o) => o.id === organizationId)) {
		event?.setSeverity('warning');
		event?.set('switchRejected', 'not a member');
		return redirect(redirectTo);
	}

	const session = await getSession(request.headers.get('Cookie'));
	session.set('currentOrganizationID', organizationId);

	// Switching organization means working in one of its projects. With none
	// the project is cleared, and the layout takes over: a project shared with
	// the user, or /projects/new to create the organization's first.
	const current = projects.find((p) => p.id === session.get('currentProjectID'));
	if (current?.organization_id !== organizationId) {
		const first = projects.find((p) => p.organization_id === organizationId);
		if (first) session.set('currentProjectID', first.id);
		else session.unset('currentProjectID');
	}

	return redirect(redirectTo, { headers: { 'Set-Cookie': await commitSession(session) } });
}

// Switching is a POST-only action; a stray GET has nothing to render.
export async function loader() {
	return redirect(FALLBACK_REDIRECT);
}
