import { afterEach, describe, expect, it, vi } from 'vitest';

import { allowlistFromEnv } from './allowlist.server';
import { allowlistPolicy, editionFromEnv, signupPolicyFromEnv } from './signup.server';

describe('editionFromEnv', () => {
	it('is selfhosted when LOGWOLF_EDITION is unset or blank', () => {
		expect(editionFromEnv({})).toBe('selfhosted');
		expect(editionFromEnv({ LOGWOLF_EDITION: '  ' })).toBe('selfhosted');
	});

	it('reads it trimmed and lowercased', () => {
		expect(editionFromEnv({ LOGWOLF_EDITION: ' SelfHosted ' })).toBe('selfhosted');
		expect(editionFromEnv({ LOGWOLF_EDITION: 'CLOUD' })).toBe('cloud');
	});

	it('refuses an edition it does not know', () => {
		expect(() => editionFromEnv({ LOGWOLF_EDITION: 'enterprise' })).toThrow('unknown edition');
	});
});

describe('allowlistPolicy', () => {
	const allowlist = allowlistFromEnv({ LOGWOLF_ALLOWED_GITHUB_USERS: 'alice', LOGWOLF_ALLOWED_GITHUB_ORGS: 'acme' });

	it('admits an allowlisted user without asking GitHub', async () => {
		const listOrgs = vi.fn(async () => []);
		expect(await allowlistPolicy(allowlist, listOrgs).maySignIn('Alice', 'token')).toBe(true);
		expect(listOrgs).not.toHaveBeenCalled();
	});

	it('asks GitHub for the orgs with the token the user signed in with', async () => {
		const listOrgs = vi.fn(async (token: string) => (token === 'bobs-token' ? ['ACME'] : []));
		const policy = allowlistPolicy(allowlist, listOrgs);

		expect(await policy.maySignIn('bob', 'bobs-token')).toBe(true);
		expect(listOrgs).toHaveBeenCalledWith('bobs-token');
		expect(await policy.maySignIn('mallory', 'mallorys-token')).toBe(false);
	});

	it('lets a failure to list the orgs through, for the caller to refuse', async () => {
		const policy = allowlistPolicy(allowlist, async () => {
			throw new Error('GitHub is down');
		});
		await expect(policy.maySignIn('bob', 'token')).rejects.toThrow('GitHub is down');
	});
});

describe('signupPolicyFromEnv', () => {
	afterEach(() => vi.restoreAllMocks());

	it('is the allowlist by default', async () => {
		const policy = signupPolicyFromEnv({ LOGWOLF_ALLOWED_GITHUB_USERS: 'alice' });
		expect(await policy.maySignIn('alice', 'token')).toBe(true);
		expect(await policy.maySignIn('bob', 'token')).toBe(false);
	});

	it('warns when the allowlist admits nobody', async () => {
		const error = vi.spyOn(console, 'error').mockImplementation(() => {});
		const policy = signupPolicyFromEnv({ LOGWOLF_EDITION: 'selfhosted' });

		expect(error).toHaveBeenCalledWith(expect.stringContaining('nobody can sign in'));
		expect(await policy.maySignIn('alice', 'token')).toBe(false);
	});

	// Running a hosted deployment on an empty allowlist would lock everyone
	// out without saying why.
	it('refuses the cloud edition, which this build does not have', () => {
		expect(() => signupPolicyFromEnv({ LOGWOLF_EDITION: 'cloud' })).toThrow('not available in this build');
	});
});
