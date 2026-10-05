import { Building2, KeyRound, LayoutDashboard, ScrollText, Settings } from 'lucide-react';
import { Link } from 'react-router';

import type { Project, UserOrganization } from '~/lib/api';

import type { Route } from '../../+types/root';
import {
	Sidebar,
	SidebarContent,
	SidebarFooter,
	SidebarGroup,
	SidebarGroupContent,
	SidebarGroupLabel,
	SidebarHeader,
	SidebarMenu,
	SidebarMenuButton,
	SidebarMenuItem,
	SidebarRail,
} from '../ui/sidebar';
import { LogoMark } from './logo';
import { OrganizationSwitcher } from './organization-switcher';
import { ProjectSwitcher } from './project-switcher';

const items = [
	{
		title: 'Dashboard',
		url: '/dashboard',
		icon: LayoutDashboard,
	},

	{
		title: 'Events',
		url: '/events',
		icon: ScrollText,
	},

	{
		title: 'Keys',
		url: '/keys',
		icon: KeyRound,
	},
] as const;

export type SidebarUser = { login: string; name: string | null; avatarUrl: string | null };

type Props = Pick<Route.ComponentProps, 'matches'> & {
	projects: Project[];
	currentProject: Project | undefined;
	organizations: UserOrganization[];
	currentOrganization: UserOrganization | undefined;
	csrfToken: string;
	user: SidebarUser;
};
export function AppSidebar({
	matches,
	projects,
	currentProject,
	organizations,
	currentOrganization,
	csrfToken,
	user,
}: Props) {
	// Settings live under the project they configure. /settings still forwards
	// there, but linking straight at the project keeps the item highlighted once
	// the page is open.
	const navItems = [
		...items,
		{
			title: 'Settings',
			url: currentProject ? `/projects/${currentProject.id}/settings` : '/settings',
			icon: Settings,
		},
	];

	return (
		<Sidebar collapsible='icon'>
			<SidebarHeader className='gap-3 pt-3'>
				<Link
					to='/dashboard'
					className='flex h-8 items-center gap-2 px-2 font-semibold tracking-tight group-data-[collapsible=icon]:px-1'
				>
					<LogoMark />
					<span className='group-data-[collapsible=icon]:hidden'>Logwolf</span>
				</Link>

				<div className='flex flex-col gap-1'>
					<OrganizationSwitcher
						organizations={organizations}
						currentOrganization={currentOrganization}
						csrfToken={csrfToken}
					/>
					<ProjectSwitcher
						projects={projects}
						currentProject={currentProject}
						organizations={organizations}
						currentOrganization={currentOrganization}
						csrfToken={csrfToken}
					/>
				</div>
			</SidebarHeader>

			<SidebarContent>
				<SidebarGroup>
					<SidebarGroupLabel>Project</SidebarGroupLabel>

					<SidebarGroupContent>
						<SidebarMenu>
							{navItems.map((item) => (
								<SidebarMenuItem key={item.title}>
									<SidebarMenuButton
										asChild
										tooltip={item.title}
										isActive={matches.some((m) => m?.pathname.includes(item.url))}
										className='text-sidebar-foreground/75 data-[active=true]:bg-primary/10 data-[active=true]:text-primary data-[active=true]:hover:bg-primary/15 data-[active=true]:hover:text-primary'
									>
										<Link to={item.url}>
											<item.icon />
											<span>{item.title}</span>
										</Link>
									</SidebarMenuButton>
								</SidebarMenuItem>
							))}
						</SidebarMenu>
					</SidebarGroupContent>
				</SidebarGroup>

				{currentOrganization && (
					<SidebarGroup>
						<SidebarGroupLabel>Organization</SidebarGroupLabel>

						<SidebarGroupContent>
							<SidebarMenu>
								<SidebarMenuItem>
									<SidebarMenuButton
										asChild
										tooltip='Organization settings'
										isActive={matches.some((m) => m?.pathname.startsWith('/organizations/'))}
										className='text-sidebar-foreground/75 data-[active=true]:bg-primary/10 data-[active=true]:text-primary data-[active=true]:hover:bg-primary/15 data-[active=true]:hover:text-primary'
									>
										<Link to={`/organizations/${currentOrganization.id}/settings`}>
											<Building2 />
											<span>Settings</span>
										</Link>
									</SidebarMenuButton>
								</SidebarMenuItem>
							</SidebarMenu>
						</SidebarGroupContent>
					</SidebarGroup>
				)}
			</SidebarContent>

			<SidebarFooter className='border-t'>
				<div className='flex items-center gap-2.5 px-1 py-1 group-data-[collapsible=icon]:px-0'>
					{user.avatarUrl ? (
						<img src={user.avatarUrl} alt='' className='size-8 shrink-0 rounded-md border' />
					) : (
						<span className='flex size-8 shrink-0 items-center justify-center rounded-md border bg-muted text-xs font-medium uppercase'>
							{user.login.slice(0, 2)}
						</span>
					)}

					<div className='grid min-w-0 flex-1 leading-tight group-data-[collapsible=icon]:hidden'>
						<span className='truncate text-sm font-medium'>{user.name || user.login}</span>
						<span className='truncate font-mono text-xs text-muted-foreground'>@{user.login}</span>
					</div>
				</div>
			</SidebarFooter>

			<SidebarRail />
		</Sidebar>
	);
}
