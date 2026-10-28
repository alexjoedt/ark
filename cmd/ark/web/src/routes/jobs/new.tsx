import { useState } from 'react';
import { useNavigate, redirect } from 'react-router';
import type { ActionFunctionArgs } from 'react-router';
import { api } from '@/lib/api';
import { useAsyncData } from '@/hooks/use-async-data';
import { useDocker } from '@/lib/contexts/docker-context';
import { ROUTES } from '@/lib/constants';
import type { CreateJobFormData } from '@/lib/schemas';
import { JobForm } from '@/components/features/job-form';
import { toast } from 'sonner';
import { ArrowLeft } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';

export async function createJobAction({ request }: ActionFunctionArgs) {
  const formData = await request.formData();
  
  const data: CreateJobFormData = {
    name: formData.get('name') as string,
    volume_name: formData.get('volume_name') as string,
    schedule: formData.get('schedule') as string,
    policy_id: formData.get('policy_id') ? Number(formData.get('policy_id')) : undefined,
    enabled: formData.get('enabled') === 'true',
  };

  try {
    await api.createJob(data);
    toast.success('Job created successfully');
    return redirect(ROUTES.JOBS);
  } catch (error: any) {
    toast.error(error?.message || 'Failed to create job');
    return { error: error?.message };
  }
}

export default function JobNewPage() {
  const { dockerAvailable, loading: dockerLoading } = useDocker();
  const { data: volumes, loading: volumesLoading } = useAsyncData(
    () => api.listVolumes(),
    [],
    undefined,
    { enabled: !dockerLoading && dockerAvailable }
  );
  const { data: policies, loading: policiesLoading } = useAsyncData(
    () => api.listPolicies(),
    []
  );
  const loading = dockerLoading || volumesLoading || policiesLoading;

  const navigate = useNavigate();
  const [isSubmitting, setIsSubmitting] = useState(false);

  const handleSubmit = async (data: CreateJobFormData) => {
    setIsSubmitting(true);
    try {
      await api.createJob(data);
      toast.success('Job created successfully');
      navigate(ROUTES.JOBS);
    } catch (error: any) {
      toast.error(error?.message || 'Failed to create job');
      setIsSubmitting(false);
    }
  };

  return (
    <div className="container mx-auto p-6 max-w-2xl">
      <div className="mb-6">
        <Button
          variant="ghost"
          size="sm"
          onClick={() => navigate(ROUTES.JOBS)}
          className="mb-2"
        >
          <ArrowLeft className="mr-2 h-4 w-4" />
          Back to Jobs
        </Button>
        <h1 className="text-3xl font-bold">Create Backup Job</h1>
        <p className="text-muted-foreground">Schedule automated backups for your volumes</p>
      </div>

      <div className="bg-card border rounded-lg p-6">
        {loading ? (
          <div className="space-y-4">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-1/3" />
          </div>
        ) : (
          <JobForm
            volumes={volumes}
            policies={policies}
            onSubmit={handleSubmit}
            onCancel={() => navigate(ROUTES.JOBS)}
            isSubmitting={isSubmitting}
          />
        )}
      </div>
    </div>
  );
}
