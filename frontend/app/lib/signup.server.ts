// Who may sign in to the dashboard is the edition's call: a self-hosted install
// admits its allowlist, the hosted one will admit sign-ups. Sign-in asks the
// SignupPolicy that LOGWOLF_EDITION picks, the way the broker asks its
// limits.Provider.

import { type Allowlist, allowlistFromEnv, isAllowed, isEmptyAllowlist, listGithubOrgs } from './allowlist.server';

export interface SignupPolicy {
	/**
	 * Whether the GitHub user `login` may sign in. `accessToken` is the token
	 * they just signed in with, for whatever the policy has to ask GitHub.
	 * Throwing is a refusal.
	 */
	maySignIn(login: string, accessToken: string): Promise<boolean>;
}

/** The editions `LOGWOLF_EDITION` may name; the broker reads the same variable. */
export const EDITIONS = ['selfhosted', 'cloud'] as const;
export type Edition = (typeof EDITIONS)[number];

/**
 * Reads `LOGWOLF_EDITION`, trimmed and lowercased: `selfhosted` when it is unset
 * or blank. Throws on a name it does not know rather than guess.
 */
export function editionFromEnv(env: Record<string, string | undefined> = process.env): Edition {
	const edition = (env.LOGWOLF_EDITION ?? '').trim().toLowerCase() || 'selfhosted';
	if (!(EDITIONS as readonly string[]).includes(edition)) {
		throw new Error(`LOGWOLF_EDITION=${edition}: unknown edition, want ${EDITIONS.join(' or ')}`);
	}
	return edition as Edition;
}

/**
 * Self-hosted sign-in: deny by default, a login must be on the users allowlist
 * or belong to an allowed org. `listOrgs` is only called when that depends on it.
 */
export function allowlistPolicy(
	allowlist: Allowlist,
	listOrgs: (accessToken: string) => Promise<string[]> = listGithubOrgs,
): SignupPolicy {
	return {
		maySignIn: (login, accessToken) => isAllowed(login, allowlist, () => listOrgs(accessToken)),
	};
}

/**
 * The SignupPolicy of the edition `LOGWOLF_EDITION` names. The cloud edition has
 * none in this build yet, so it throws rather than fall back to the allowlist.
 */
export function signupPolicyFromEnv(env: Record<string, string | undefined> = process.env): SignupPolicy {
	const edition = editionFromEnv(env);
	switch (edition) {
		case 'selfhosted': {
			const allowlist = allowlistFromEnv(env);
			if (isEmptyAllowlist(allowlist)) {
				console.error(
					'Neither LOGWOLF_ALLOWED_GITHUB_USERS nor LOGWOLF_ALLOWED_GITHUB_ORGS is set: nobody can sign in to the dashboard.',
				);
			}
			return allowlistPolicy(allowlist);
		}
		case 'cloud':
			throw new Error('LOGWOLF_EDITION=cloud: the cloud edition is not available in this build');
	}
}
