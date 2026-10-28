import { z } from 'zod';

// Restore
export const restoreSchema = z.object({
  volume_name: z.string().min(1),
  snapshot_id: z.string().min(1),
  force: z.boolean().default(false),
});
export type RestoreFormData = z.infer<typeof restoreSchema>;

// Authentication
export const loginSchema = z.object({
  username: z.string().min(1, 'Username is required'),
  password: z.string().min(1, 'Password is required'),
});
export type LoginFormData = z.infer<typeof loginSchema>;

// Jobs
export const createJobSchema = z.object({
  name: z.string().min(1, 'Name is required').max(100),
  volume_name: z.string().min(1, 'Volume is required'),
  schedule: z.string().min(1, 'Schedule is required')
    .regex(/^(@(hourly|daily|weekly|monthly|yearly)|((\*|[0-9,\-\/]+)\s+){4}(\*|[0-9,\-\/]+))$/,
      'Invalid cron expression'),
  policy_id: z.number().optional(),
  enabled: z.boolean().default(true),
});
export type CreateJobFormData = z.infer<typeof createJobSchema>;

// Policies
export const createPolicySchema = z.object({
  name: z.string().min(1, 'Name is required').max(100),
  description: z.string().max(500).optional(),
  keep_days: z.number().min(1).max(365).optional(),
  keep_count: z.number().min(1).max(1000).optional(),
}).refine(data => data.keep_days || data.keep_count, {
  message: 'At least one retention rule is required',
});
export type CreatePolicyFormData = z.infer<typeof createPolicySchema>;

// Users
export const createUserSchema = z.object({
  username: z.string().min(3, 'Username must be at least 3 characters').max(50),
  email: z.string().email('Invalid email address'),
  password: z.string().min(8, 'Password must be at least 8 characters'),
  role: z.enum(['admin', 'user']),
});
export type CreateUserFormData = z.infer<typeof createUserSchema>;

export const changePasswordSchema = z.object({
  password: z.string().min(8, 'Password must be at least 8 characters'),
  confirmPassword: z.string(),
}).refine(data => data.password === data.confirmPassword, {
  message: 'Passwords do not match',
  path: ['confirmPassword'],
});
export type ChangePasswordFormData = z.infer<typeof changePasswordSchema>;

// Backup
export const backupSchema = z.object({
  volume_name: z.string().min(1),
  comment: z.string().max(200).optional(),
});
