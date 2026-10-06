import { CircleAlert } from 'lucide-react';
import { Link } from 'react-router';

import { Alert, AlertDescription, AlertTitle } from '~/components/ui/alert';
import { locale } from '~/lib/locale';
import { formatRenewal } from '~/lib/organizations';

/** The current organization's monthly events, used up, as the layout loader tells it. */
export type QuotaExceeded = {
	organizationId: string;
	organizationName: string;
	monthlyEvents: number;
	/** When the quota renews, as an ISO timestamp: the first instant of next month, in UTC. */
	renewsAt: string;
};

/**
 * Says, over every page, that the current organization's events are being
 * refused: the SDK's errors are easy to miss, and nothing else on the page
 * would explain why new events stopped arriving.
 */
export function QuotaBanner({ quota }: { quota: QuotaExceeded }) {
	return (
		<Alert variant='destructive' className='rounded-none border-x-0 border-t-0'>
			<CircleAlert />
			<AlertTitle>{quota.organizationName} has used its monthly events</AlertTitle>
			<AlertDescription>
				<p>
					All {quota.monthlyEvents.toLocaleString(locale)} events of this month are used. New events from its
					projects are refused until {formatRenewal(quota.renewsAt, locale)}.{' '}
					<Link to={`/organizations/${quota.organizationId}/settings`} className='font-medium underline'>
						See plan and usage
					</Link>
				</p>
			</AlertDescription>
		</Alert>
	);
}
