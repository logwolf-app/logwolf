/**
 * Keeps a caller-supplied redirect target on this origin — an absolute URL or
 * a protocol-relative path would turn a switcher into an open redirect.
 */
export function safeRedirect(to: string | undefined, fallback = '/dashboard'): string {
	if (!to?.startsWith('/') || to.startsWith('//') || to.startsWith('/\\')) return fallback;
	return to;
}
