import { Check } from 'lucide-react';
import { useFetcher } from 'react-router';

import { type SettingsActionResult, useSuccessToast } from '~/components/settings/action-result';
import { SettingsFooter, SettingsRow } from '~/components/settings/settings-row';
import { Alert, AlertTitle } from '~/components/ui/alert';
import { Button } from '~/components/ui/button';
import { Card } from '~/components/ui/card';
import { Field, FieldGroup, FieldLabel } from '~/components/ui/field';
import { Input } from '~/components/ui/input';
import { useCsrfToken } from '~/hooks/use-csrf-token';
import type { UserOrganization } from '~/lib/api';

type Props = { organization: UserOrganization; canEdit: boolean };

export function GeneralSection({ organization, canEdit }: Props) {
	const csrfToken = useCsrfToken();
	const fetcher = useFetcher<SettingsActionResult>();
	useSuccessToast(fetcher.data);

	return (
		<SettingsRow title='General' description='How this organization is named across the dashboard.'>
			<Card className='gap-0 overflow-hidden py-0'>
				<fetcher.Form method='post'>
					<FieldGroup className='p-5'>
						{fetcher.data?.error && (
							<Alert variant='destructive'>
								<AlertTitle>{fetcher.data.error}</AlertTitle>
							</Alert>
						)}

						<input type='hidden' name='_csrf' value={csrfToken} />
						<input type='hidden' name='intent' value='rename' />

						<Field>
							<FieldLabel htmlFor='name'>Name</FieldLabel>

							{/* Keyed on the organization so opening another one's settings
							    doesn't leave the previous name in an uncontrolled input. */}
							<Input
								key={organization.id}
								id='name'
								name='name'
								type='text'
								defaultValue={organization.name}
								disabled={!canEdit}
								required
								className='max-w-sm'
							/>
						</Field>
					</FieldGroup>

					<SettingsFooter hint={canEdit ? undefined : 'Only an owner or an admin can rename this organization.'}>
						{canEdit && (
							<Button type='submit' size='sm' disabled={fetcher.state !== 'idle'}>
								<Check />
								Save
							</Button>
						)}
					</SettingsFooter>
				</fetcher.Form>
			</Card>
		</SettingsRow>
	);
}
