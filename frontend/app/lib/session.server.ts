import { createCookieSessionStorage } from 'react-router';

import { unsealToken } from './token.server';

type SessionData = {
	githubUser: {
		/** GitHub's numeric user ID, which a rename does not change. */
		id: number;
		/** The login, in GitHub's casing, for display. */
		login: string;
		name: string;
		avatarUrl: string;
	};
	csrfToken: string;
	currentProjectID: string;
	currentOrganizationID: string;
	/** The user's GitHub OAuth token, sealed (see token.server). */
	githubToken: string;
};

export const sessionStorage = createCookieSessionStorage<SessionData>({
	cookie: {
		name: '__logwolf_session',
		httpOnly: true,
		secure: process.env.NODE_ENV === 'production',
		sameSite: 'strict',
		secrets: [process.env.SESSION_SECRET!],
		maxAge: 60 * 60 * 24 * 7, // 1 week
	},
});

export const { getSession, commitSession, destroySession } = sessionStorage;

/**
 * Reads the project the user is currently working in. Undefined means the user
 * has no reachable project — the layout loader clears the id whenever the
 * stored project is gone or the user lost access to it.
 */
export async function getCurrentProjectID(request: Request): Promise<string | undefined> {
	const session = await getSession(request.headers.get('Cookie'));
	return session.get('currentProjectID');
}

/**
 * Reads the organization the user is currently working in. Undefined means the
 * user is a member of no organization — the layout loader keeps the id to one
 * they still belong to, like the project's.
 */
export async function getCurrentOrganizationID(request: Request): Promise<string | undefined> {
	const session = await getSession(request.headers.get('Cookie'));
	return session.get('currentOrganizationID');
}

/**
 * Reads the signed-in user's GitHub token, or undefined if the session has
 * none: it was created before tokens were kept, or the token cannot be unsealed.
 */
export async function getGithubToken(request: Request): Promise<string | undefined> {
	const session = await getSession(request.headers.get('Cookie'));
	return unsealToken(session.get('githubToken'));
}
