import {
	LogwolfEventSchema,
	type CreateLogwolfEventDTOSchema,
	type LogwolfEventData,
	type Pagination,
} from '@logwolf/client-js';
import z from 'zod';

type ApiResponse<T> = { message: string } & ({ error: true; data: never } | { error: false; data: T });

/**
 * An event the way the broker takes it in: `data` already encoded as a JSON
 * string. `LogwolfEvent.toObject()` produces exactly this shape.
 */
export type EncodedEvent = z.input<typeof CreateLogwolfEventDTOSchema>;

/** What a key may do on the public /logs routes. */
export const API_KEY_SCOPES = ['ingest', 'read', 'delete'] as const;
export type ApiKeyScope = (typeof API_KEY_SCOPES)[number];

type ApiKey = {
	id: string;
	project_id: string;
	prefix: string;
	scopes: ApiKeyScope[];
	/** Created before scopes existed: it keeps the full access it always had. */
	legacy?: boolean;
	active: boolean;
	created_at: string;
	revoked_at: string;
};

export type CreatedApiKey = { key: string; prefix: string; id: string; scopes: ApiKeyScope[] };

export type Project = {
	id: string;
	name: string;
	slug: string;
	/** The organization the project belongs to; every project has one once the logger has migrated it. */
	organization_id?: string;
	created_at: string;
};

export type ProjectRole = 'owner' | 'member';

/** A project together with the role the requesting user holds in it. */
export type UserProject = Project & { role: ProjectRole };

/**
 * A row of the project_members collection, as returned by the broker. `id` is
 * the membership's own, which the member routes take. `user_id` is the member's
 * GitHub user ID, who the membership belongs to; memberships stored before user
 * IDs have none, and are matched by `github_login` until they are linked.
 * `github_login` is for display: the login at the member's last sign-in, or the
 * one they were added under.
 */
export type ProjectMember = {
	id: string;
	project_id: string;
	user_id?: number;
	github_login: string;
	role: ProjectRole;
	created_at: string;
};

export type OrganizationRole = 'owner' | 'admin' | 'member';

/**
 * An organization owns projects and holds the plan their limits come from.
 * `plan` is the plan's name; `getOrganizationPlan` resolves its limits.
 */
export type Organization = {
	id: string;
	name: string;
	plan: string;
	created_at: string;
};

/** An organization together with the role the requesting user holds in it. */
export type UserOrganization = Organization & { role: OrganizationRole };

/**
 * A row of the organization_members collection. Unlike project memberships,
 * every one names its user by GitHub user ID; `github_login` is for display.
 */
export type OrganizationMember = {
	id: string;
	organization_id: string;
	user_id: number;
	github_login: string;
	role: OrganizationRole;
	created_at: string;
};

/**
 * An organization's plan, as the broker's `limits.Provider` resolves it, and
 * how much of it the organization uses. 0 in a limit means the plan sets none;
 * for `max_retention_days`, forever. `usage.events` is the events its projects
 * have ingested this calendar month (UTC), as far as the brokers have flushed
 * them: a minute behind at most.
 */
export type OrganizationPlan = {
	plan: {
		name: string;
		monthly_events: number;
		max_retention_days: number;
		max_projects: number;
		max_members: number;
	};
	usage: { projects: number; members: number; events: number };
};

/** Events and their bytes, added up. */
export type UsageTotals = { events: number; bytes: number };

/**
 * One project's line in its organization's usage: the events accepted for it
 * this month and their bytes, and what it stores as last measured.
 * `storage_measured_at` is the zero time (`0001-01-01T00:00:00Z`) until the
 * logger first measures it.
 */
export type ProjectUsage = UsageTotals & {
	project_id: string;
	name: string;
	storage: UsageTotals;
	storage_measured_at: string;
};

/**
 * What each of an organization's projects used in `month` (UTC), the most
 * events first, every project in it included. `deleted` adds up its deleted
 * projects, whose events still count toward the monthly quota, so the lines add
 * up to `OrganizationPlan.usage.events`.
 */
export type OrganizationProjectsUsage = {
	month: string;
	projects: ProjectUsage[];
	deleted: UsageTotals;
};

/** The signed-in user the broker acts for: their GitHub user ID, and their login. */
export type ApiUser = { id: number; login: string };

/** A GitHub user to add to a project, by user ID, with the login to show. */
export type Invitee = { id: number; login: string };

/**
 * A project's retention in days, 0 being forever, and the values it may pick:
 * the broker asks its edition's `limits.Provider`, so they differ by plan.
 */
export type Retention = { days: number; choices: number[] };

/**
 * A dashboard user as the broker stores them, keyed by their GitHub user ID;
 * `github_login` is the login at their last sign-in, lowercased.
 */
export type User = {
	id: string;
	github_id: number;
	github_login: string;
	email: string;
	created_at: string;
};

export type Metrics = {
	total_events: number;
	total_errors: number;
	total_critical: number;
	avg_duration_ms: number;
	events_last_24h: number;
	errors_last_24h: number;
	top_tags: { tag: string; count: number }[] | null;
};

export interface IApi {
	/** Records a sign-in of the signed-in user, with their public email. */
	upsertCurrentUser(email: string): Promise<User>;
	getProjects(): Promise<UserProject[]>;
	/**
	 * Creates a project owned by the signed-in user in `organizationId`, or,
	 * with none, in the deployment's Default organization.
	 */
	createProject(name: string, slug: string, organizationId?: string): Promise<Project>;
	updateProject(id: string, name: string): Promise<Project>;
	deleteProject(id: string): Promise<void>;
	getMembers(projectId: string): Promise<ProjectMember[]>;
	addMember(projectId: string, invitee: Invitee, role: ProjectRole): Promise<void>;
	/** `memberId` is the membership's `id`. */
	updateMemberRole(projectId: string, memberId: string, role: ProjectRole): Promise<void>;
	/** `memberId` is the membership's `id`. */
	removeMember(projectId: string, memberId: string): Promise<void>;
	getKeys(projectId: string): Promise<ApiKey[]>;
	createKey(projectId: string, scopes: ApiKeyScope[]): Promise<CreatedApiKey>;
	deleteKey(projectId: string, id: string): Promise<void>;
	getRetention(projectId: string): Promise<Retention>;
	updateRetention(projectId: string, days: number): Promise<Retention>;
	getMetrics(projectId: string): Promise<Metrics>;
	getLogs(projectId: string, p: Pagination): Promise<LogwolfEventData[]>;
	getLog(projectId: string, id: string): Promise<LogwolfEventData>;
	createLog(projectId: string, event: EncodedEvent): Promise<void>;
	deleteLog(projectId: string, id: string): Promise<void>;
	getOrganizations(): Promise<UserOrganization[]>;
	updateOrganization(id: string, name: string): Promise<UserOrganization>;
	getOrganizationPlan(id: string): Promise<OrganizationPlan>;
	/** For owners and admins only: it names every project in the organization. */
	getOrganizationUsage(id: string): Promise<OrganizationProjectsUsage>;
	getOrganizationMembers(id: string): Promise<OrganizationMember[]>;
	addOrganizationMember(id: string, invitee: Invitee, role: OrganizationRole): Promise<void>;
	/** `memberId` is the membership's `id`. */
	updateOrganizationMemberRole(id: string, memberId: string, role: OrganizationRole): Promise<void>;
	/** `memberId` is the membership's `id`. */
	removeOrganizationMember(id: string, memberId: string): Promise<void>;
}

export class Api implements IApi {
	constructor(
		private readonly baseUrl: string,
		private readonly secret: string,
		private readonly user: ApiUser,
	) {}

	// The broker authorizes by the user ID; the login names memberships stored
	// before user IDs, and is what the user is called.
	private internalHeaders(extra?: Record<string, string>): Record<string, string> {
		return {
			'X-Internal-Secret': this.secret,
			'X-User-ID': String(this.user.id),
			'X-User-Login': this.user.login,
			...extra,
		};
	}

	public async upsertCurrentUser(email: string): Promise<User> {
		const res = await fetch(`${this.baseUrl}users/me`, {
			method: 'PUT',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ email }),
		});
		const json = (await res.json()) as ApiResponse<User>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async getProjects(): Promise<UserProject[]> {
		const res = await fetch(`${this.baseUrl}projects`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<UserProject[]>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async createProject(name: string, slug: string, organizationId?: string): Promise<Project> {
		const path = organizationId ? `organizations/${organizationId}/projects` : 'projects';
		const res = await fetch(`${this.baseUrl}${path}`, {
			method: 'POST',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ name, slug }),
		});
		const json = (await res.json()) as ApiResponse<Project>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async updateProject(id: string, name: string): Promise<Project> {
		const res = await fetch(`${this.baseUrl}projects/${id}`, {
			method: 'PATCH',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ name }),
		});
		const json = (await res.json()) as ApiResponse<Project>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async deleteProject(id: string): Promise<void> {
		const res = await fetch(`${this.baseUrl}projects/${id}`, {
			method: 'DELETE',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async getMembers(projectId: string): Promise<ProjectMember[]> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/members`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<ProjectMember[]>;
		if (json.error) throw new Error(json.message);

		return json.data ?? [];
	}

	public async addMember(projectId: string, invitee: Invitee, role: ProjectRole): Promise<void> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/members`, {
			method: 'POST',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ login: invitee.login, user_id: invitee.id, role }),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async updateMemberRole(projectId: string, memberId: string, role: ProjectRole): Promise<void> {
		// Encoded for the same reason as in removeMember.
		const res = await fetch(`${this.baseUrl}projects/${projectId}/members/${encodeURIComponent(memberId)}`, {
			method: 'PATCH',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ role }),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async removeMember(projectId: string, memberId: string): Promise<void> {
		// Membership ids are URL-safe, but the value reaches us from a form field —
		// encoding it keeps a hand-crafted one from reshaping the path.
		const res = await fetch(`${this.baseUrl}projects/${projectId}/members/${encodeURIComponent(memberId)}`, {
			method: 'DELETE',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async getKeys(projectId: string): Promise<ApiKey[]> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/keys`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<ApiKey[]>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	/** An empty `scopes` leaves the choice to the broker, which gives the key ingest only. */
	public async createKey(projectId: string, scopes: ApiKeyScope[]): Promise<CreatedApiKey> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/keys`, {
			method: 'POST',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ scopes }),
		});
		const json = (await res.json()) as ApiResponse<CreatedApiKey>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	/** Revokes a key of the project; another project's key is "key not found". */
	public async deleteKey(projectId: string, id: string): Promise<void> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/keys/${encodeURIComponent(id)}`, {
			method: 'DELETE',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
		return json.data;
	}

	public async getRetention(projectId: string): Promise<Retention> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/retention`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<Retention>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async updateRetention(projectId: string, days: number): Promise<Retention> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/retention`, {
			method: 'PATCH',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ days }),
		});
		const json = (await res.json()) as ApiResponse<Retention>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async getMetrics(projectId: string): Promise<Metrics> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/metrics`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<Metrics>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async getLogs(projectId: string, p: Pagination): Promise<LogwolfEventData[]> {
		const url = new URL(`${this.baseUrl}projects/${projectId}/logs`);
		url.searchParams.set('page', String(p.page));
		url.searchParams.set('pageSize', String(p.pageSize));
		const res = await fetch(url.toString(), {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<unknown[]>;
		if (json.error) throw new Error(json.message);

		return LogwolfEventSchema.array().parse(json.data ?? []);
	}

	public async getLog(projectId: string, id: string): Promise<LogwolfEventData> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/logs/${encodeURIComponent(id)}`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<unknown>;
		if (json.error) throw new Error(json.message);

		return LogwolfEventSchema.parse(json.data);
	}

	public async createLog(projectId: string, event: EncodedEvent): Promise<void> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/logs`, {
			method: 'POST',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify(event),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async deleteLog(projectId: string, id: string): Promise<void> {
		const res = await fetch(`${this.baseUrl}projects/${projectId}/logs/${encodeURIComponent(id)}`, {
			method: 'DELETE',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async getOrganizations(): Promise<UserOrganization[]> {
		const res = await fetch(`${this.baseUrl}organizations`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<UserOrganization[]>;
		if (json.error) throw new Error(json.message);

		return json.data ?? [];
	}

	public async updateOrganization(id: string, name: string): Promise<UserOrganization> {
		const res = await fetch(`${this.baseUrl}organizations/${id}`, {
			method: 'PATCH',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ name }),
		});
		const json = (await res.json()) as ApiResponse<UserOrganization>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async getOrganizationPlan(id: string): Promise<OrganizationPlan> {
		const res = await fetch(`${this.baseUrl}organizations/${id}/plan`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<OrganizationPlan>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async getOrganizationUsage(id: string): Promise<OrganizationProjectsUsage> {
		const res = await fetch(`${this.baseUrl}organizations/${id}/usage`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<OrganizationProjectsUsage>;
		if (json.error) throw new Error(json.message);

		return json.data;
	}

	public async getOrganizationMembers(id: string): Promise<OrganizationMember[]> {
		const res = await fetch(`${this.baseUrl}organizations/${id}/members`, {
			method: 'GET',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<OrganizationMember[]>;
		if (json.error) throw new Error(json.message);

		return json.data ?? [];
	}

	public async addOrganizationMember(id: string, invitee: Invitee, role: OrganizationRole): Promise<void> {
		const res = await fetch(`${this.baseUrl}organizations/${id}/members`, {
			method: 'POST',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ login: invitee.login, user_id: invitee.id, role }),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async updateOrganizationMemberRole(id: string, memberId: string, role: OrganizationRole): Promise<void> {
		// Encoded like project membership ids: the value reaches us from a form field.
		const res = await fetch(`${this.baseUrl}organizations/${id}/members/${encodeURIComponent(memberId)}`, {
			method: 'PATCH',
			headers: this.internalHeaders({ 'Content-Type': 'application/json' }),
			body: JSON.stringify({ role }),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}

	public async removeOrganizationMember(id: string, memberId: string): Promise<void> {
		const res = await fetch(`${this.baseUrl}organizations/${id}/members/${encodeURIComponent(memberId)}`, {
			method: 'DELETE',
			headers: this.internalHeaders(),
		});
		const json = (await res.json()) as ApiResponse<void>;
		if (json.error) throw new Error(json.message);
	}
}

/**
 * Create a request-scoped API client acting for `user`, which must come from
 * the authenticated GitHub session (its `githubUser`).
 */
export function createApi(user: ApiUser): IApi {
	return new Api(process.env.API_URL!, process.env.INTERNAL_API_SECRET!, { id: user.id, login: user.login });
}
