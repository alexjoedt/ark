import { useForm, Controller } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { createJobSchema } from '@/lib/schemas';
import type { CreateJobFormData } from '@/lib/schemas';
import type { BackupJob, Volume, BackupPolicy } from '@/lib/types';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Switch } from '@/components/ui/switch';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import { CronInput } from './cron-input';
import { Loader2 } from 'lucide-react';

interface JobFormProps {
  defaultValues?: Partial<BackupJob>;
  volumes: Volume[];
  policies: BackupPolicy[];
  onSubmit: (data: CreateJobFormData) => void;
  onCancel: () => void;
  isSubmitting: boolean;
}

export function JobForm({
  defaultValues,
  volumes,
  policies,
  onSubmit,
  onCancel,
  isSubmitting,
}: JobFormProps) {
  const {
    register,
    handleSubmit,
    control,
    formState: { errors },
  } = useForm<CreateJobFormData>({
    resolver: zodResolver(createJobSchema),
    defaultValues: {
      name: defaultValues?.name || '',
      volume_name: defaultValues?.volume_name || '',
      schedule: defaultValues?.schedule || '',
      policy_id: defaultValues?.policy_id,
      enabled: defaultValues?.enabled ?? true,
    },
  });

  return (
    <form onSubmit={handleSubmit(onSubmit)} className="space-y-6">
      <div className="space-y-4">
        <div>
          <Label htmlFor="name">Job Name</Label>
          <Input
            id="name"
            {...register('name')}
            placeholder="e.g., Daily Database Backup"
            disabled={isSubmitting}
          />
          {errors.name && (
            <p className="text-sm text-destructive mt-1">{errors.name.message}</p>
          )}
        </div>

        <div>
          <Label htmlFor="volume_name">Volume</Label>
          <Controller
            name="volume_name"
            control={control}
            render={({ field }) => (
              <Select
                value={field.value}
                onValueChange={field.onChange}
                disabled={isSubmitting}
              >
                <SelectTrigger id="volume_name">
                  <SelectValue placeholder="Select a volume" />
                </SelectTrigger>
                <SelectContent>
                  {volumes.map((volume) => (
                    <SelectItem key={volume.name} value={volume.name}>
                      {volume.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          {errors.volume_name && (
            <p className="text-sm text-destructive mt-1">{errors.volume_name.message}</p>
          )}
        </div>

        <Controller
          name="schedule"
          control={control}
          render={({ field }) => (
            <CronInput
              value={field.value}
              onChange={field.onChange}
              error={errors.schedule?.message}
            />
          )}
        />

        <div>
          <Label htmlFor="policy_id">Retention Policy (Optional)</Label>
          <Controller
            name="policy_id"
            control={control}
            render={({ field }) => (
              <Select
                value={field.value?.toString() || 'none'}
                onValueChange={(value) => field.onChange(value === 'none' ? undefined : Number(value))}
                disabled={isSubmitting}
              >
                <SelectTrigger id="policy_id">
                  <SelectValue placeholder="No retention policy" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none">No policy</SelectItem>
                  {policies.map((policy) => (
                    <SelectItem key={policy.id} value={policy.id.toString()}>
                      {policy.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
          <p className="text-sm text-muted-foreground mt-1">
            Automatically delete old backups based on the policy rules
          </p>
          {errors.policy_id && (
            <p className="text-sm text-destructive mt-1">{errors.policy_id.message}</p>
          )}
        </div>

        <div className="flex items-center justify-between">
          <div>
            <Label htmlFor="enabled">Enabled</Label>
            <p className="text-sm text-muted-foreground">
              Job will run on schedule when enabled
            </p>
          </div>
          <Controller
            name="enabled"
            control={control}
            render={({ field }) => (
              <Switch
                id="enabled"
                checked={field.value}
                onCheckedChange={field.onChange}
                disabled={isSubmitting}
              />
            )}
          />
        </div>
      </div>

      <div className="flex gap-3 justify-end pt-4 border-t">
        <Button
          type="button"
          variant="outline"
          onClick={onCancel}
          disabled={isSubmitting}
        >
          Cancel
        </Button>
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
          {defaultValues ? 'Update Job' : 'Create Job'}
        </Button>
      </div>
    </form>
  );
}
