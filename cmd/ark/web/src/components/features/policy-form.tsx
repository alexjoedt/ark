import { useForm, Controller } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { createPolicySchema } from '@/lib/schemas';
import type { CreatePolicyFormData } from '@/lib/schemas';
import type { BackupPolicy } from '@/lib/types';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Loader2 } from 'lucide-react';

interface PolicyFormProps {
  defaultValues?: Partial<BackupPolicy>;
  onSubmit: (data: CreatePolicyFormData) => void;
  onCancel: () => void;
  isSubmitting: boolean;
}

export function PolicyForm({
  defaultValues,
  onSubmit,
  onCancel,
  isSubmitting,
}: PolicyFormProps) {
  const {
    register,
    handleSubmit,
    control,
    formState: { errors },
  } = useForm<CreatePolicyFormData>({
    resolver: zodResolver(createPolicySchema),
    defaultValues: {
      name: defaultValues?.name || '',
      description: defaultValues?.description || '',
      keep_count: defaultValues?.keep_count,
      keep_days: defaultValues?.keep_days,
    },
  });

  return (
    <form onSubmit={handleSubmit(onSubmit)} className="space-y-6">
      <div className="space-y-4">
        <div>
          <Label htmlFor="name">Policy Name</Label>
          <Input
            id="name"
            {...register('name')}
            placeholder="e.g., Keep 30 Days"
            disabled={isSubmitting}
          />
          {errors.name && (
            <p className="text-sm text-destructive mt-1">{errors.name.message}</p>
          )}
        </div>

        <div>
          <Label htmlFor="description">Description (Optional)</Label>
          <Input
            id="description"
            {...register('description')}
            placeholder="e.g., Retain backups for a month"
            disabled={isSubmitting}
          />
          {errors.description && (
            <p className="text-sm text-destructive mt-1">{errors.description.message}</p>
          )}
        </div>

        <div>
          <Label htmlFor="keep_count">Keep Count (Optional)</Label>
          <Controller
            name="keep_count"
            control={control}
            render={({ field }) => (
              <Input
                id="keep_count"
                type="number"
                min={1}
                max={1000}
                placeholder="e.g., 10"
                disabled={isSubmitting}
                value={field.value ?? ''}
                onChange={(e) =>
                  field.onChange(e.target.value === '' ? undefined : Number(e.target.value))
                }
              />
            )}
          />
          <p className="text-sm text-muted-foreground mt-1">
            Maximum number of backup archives to keep
          </p>
          {errors.keep_count && (
            <p className="text-sm text-destructive mt-1">{errors.keep_count.message}</p>
          )}
        </div>

        <div>
          <Label htmlFor="keep_days">Keep Days (Optional)</Label>
          <Controller
            name="keep_days"
            control={control}
            render={({ field }) => (
              <Input
                id="keep_days"
                type="number"
                min={1}
                max={365}
                placeholder="e.g., 30"
                disabled={isSubmitting}
                value={field.value ?? ''}
                onChange={(e) =>
                  field.onChange(e.target.value === '' ? undefined : Number(e.target.value))
                }
              />
            )}
          />
          <p className="text-sm text-muted-foreground mt-1">
            Delete archives older than this many days
          </p>
          {errors.keep_days && (
            <p className="text-sm text-destructive mt-1">{errors.keep_days.message}</p>
          )}
        </div>

        {errors.root && (
          <p className="text-sm text-destructive">{errors.root.message}</p>
        )}
      </div>

      <div className="flex gap-3 pt-2">
        <Button type="submit" disabled={isSubmitting}>
          {isSubmitting && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
          {defaultValues?.id ? 'Update Policy' : 'Create Policy'}
        </Button>
        <Button type="button" variant="outline" onClick={onCancel} disabled={isSubmitting}>
          Cancel
        </Button>
      </div>
    </form>
  );
}
