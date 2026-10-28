export const ROUTES = {
  LOGIN: '/login',
  DASHBOARD: '/',
  VOLUMES: '/volumes',
  VOLUME_DETAIL: (name: string) => `/volumes/${name}`,
  SNAPSHOTS: '/snapshots',
  SNAPSHOT_DETAIL: (id: string) => `/snapshots/${id}`,
  JOBS: '/jobs',
  JOB_NEW: '/jobs/new',
  JOB_DETAIL: (id: number) => `/jobs/${id}`,
  POLICIES: '/policies',
  POLICY_NEW: '/policies/new',
  POLICY_DETAIL: (id: number) => `/policies/${id}`,
  USERS: '/users',
  USER_NEW: '/users/new',
} as const;

export const CRON_PRESETS = [
  { label: 'Every hour', value: '0 * * * *' },
  { label: 'Daily at midnight', value: '0 0 * * *' },
  { label: 'Daily at 3 AM', value: '0 3 * * *' },
  { label: 'Weekly (Sunday)', value: '0 0 * * 0' },
  { label: 'Monthly (1st)', value: '0 0 1 * *' },
] as const;

export const WS_RECONNECT_DELAY = 3000;
export const HISTORY_DEFAULT_LIMIT = 50;
