import { redirect } from 'react-router';

import { createApi } from './api';
import { commitSession, destroySession, getSession } from './session.server';
import { signupPolicyFromEnv } from './signup.server';
import { sealToken } from './token.server';

const GITHUB_CLIENT_ID = process.env.GITHUB_CLIENT_ID!;
const GITHUB_CLIENT_SECRET = process.env.GITHUB_CLIENT_SECRET!;
// Picked by LOGWOLF_EDITION once, at startup, which an edition this build does
// not have fails.
const SIGNUP_POLICY = signupPolicyFromEnv();

export function getGitHubAuthURL() {
	const params = new URLSearchParams({
		client_id: GITHUB_CLIENT_ID,
		scope: 'read:user read:org',
	});
	return `https://github.com/login/oauth/authorize?${params}`;
}

export async function handleGitHubCallback(code: string, request: Request) {
	// Exchange code for token
	const tokenRes = await fetch('https://github.com/login/oauth/access_token', {
		method: 'POST',
		headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
		body: JSON.stringify({ client_id: GITHUB_CLIENT_ID, client_secret: GITHUB_CLIENT_SECRET, code }),
	});
	const { access_token } = await tokenRes.json();

	// Fetch user
	const userRes = await fetch('https://api.github.com/user', {
		headers: { Authorization: `Bearer ${access_token}`, Accept: 'application/json' },
	});
	const user = await userRes.json();

	// Deny by default: the edition's policy must admit the login (self-hosted,
	// it must be allowlisted or belong to an allowed org).
	let allowed = false;
	try {
		allowed = await SIGNUP_POLICY.maySignIn(user.login, access_token);
	} catch (err) {
		console.error('Could not check the sign-up policy', err);
	}
	if (!allowed) throw redirect('/auth?error=unauthorized');

	// Record the sign-in: the user is keyed by their GitHub ID, so a rename
	// refreshes their stored login without making them someone else. Who may sign
	// in is still decided by login, above. A sign-in that cannot be recorded does
	// not go through.
	try {
		await createApi(user.login).upsertCurrentUser(user.id, typeof user.email === 'string' ? user.email : '');
	} catch (err) {
		console.error('Could not record the sign-in', err);
		throw redirect('/auth?error=unavailable');
	}

	// Set session. The login keeps GitHub's casing for display; the broker
	// normalizes it before matching memberships. The ID is GitHub's, which a
	// rename does not change.
	const session = await getSession(request.headers.get('Cookie'));
	session.set('githubUser', {
		id: user.id,
		login: user.login,
		name: user.name,
		avatarUrl: user.avatar_url,
	});
	// Kept, sealed, for checking invitees' org membership: GitHub shows private
	// members only to a token of someone inside the org. It carries the scopes
	// sign-in asked for, read:user and read:org, and nothing more.
	if (typeof access_token === 'string' && access_token) session.set('githubToken', sealToken(access_token));

	return redirect('/dashboard', {
		headers: { 'Set-Cookie': await commitSession(session) },
	});
}

export async function requireAuth(request: Request) {
	const session = await getSession(request.headers.get('Cookie'));
	const user = session.get('githubUser');
	// A session from before sign-in kept the user's ID has none: signing in
	// again records the user and adds it.
	if (!user || typeof user.id !== 'number') throw redirect('/auth');
	return user;
}

export async function logout(request: Request) {
	const session = await getSession(request.headers.get('Cookie'));
	return redirect('/', {
		headers: { 'Set-Cookie': await destroySession(session) },
	});
}
