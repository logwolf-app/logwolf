import { useRouteLoaderData } from 'react-router';

import type { loader as layoutLoader } from '../pages/layout';

/**
 * Organizations the signed-in user is a member of, plus the one they are
 * working in, read from the layout loader like `useProjects`.
 */
export function useOrganizations() {
	const layoutData = useRouteLoaderData<typeof layoutLoader>('pages/layout');

	return {
		organizations: layoutData?.organizations ?? [],
		currentOrganization: layoutData?.currentOrganization,
	};
}
