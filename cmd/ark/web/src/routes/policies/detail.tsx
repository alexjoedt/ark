import { useState } from 'react';
import { useNavigate, useParams } from 'react-router';
import { api } from '@/lib/api';
import { useAsyncData } from '@/hooks/use-async-data';
import { ROUTES } from '@/lib/constants';
import type { CreatePolicyFormData } from '@/lib/schemas';
import { PolicyForm } from '@/components/features/policy-form';
import { DeleteDialog } from '@/components/features/delete-dialog';
import { toast } from 'sonner';
import { ArrowLeft, Trash } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  CardDescription,
} from '@/components/ui/card';

export default function PolicyDetailPage() {
  const params = useParams();
  const id = Number(params.id);

  const { data: policy, loading: policyLoading } = useAsyncData(
    () => api.getPolicy(id),
    null as any,
    String(id)
  );
  const { data: jobs, loading: jobsLoading } = useAsyncData(
    () => api.listJobs(),
    []
  );
  const loading = policyLoading || jobsLoading;

  const navigate = useNavigate();
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [showDeleteDialog, setShowDeleteDialog] = useState(false);
  const [isDeleting, setIsDeleting] = useState(false);

  const linkedJobs = jobs.filter((j) => j.policy_id === id);

  const handleSubmit = async (data: CreatePolicyFormData) => {
    setIsSubmitting(true);
    try {
      await api.updatePolicy(id, data);
      toast.success('Policy updated successfully');
      window.location.reload();
    } catch (error: any) {
      toast.error(error?.message || 'Failed to update policy');
      setIsSubmitting(false);
    }
  };

  const handleDelete = async () => {
    if (!policy) return;
    setIsDeleting(true);
    try {
      await api.deletePolicy(policy.id);
      toast.success('Policy deleted');
      navigate(ROUTES.POLICIES);
    } catch (error: any) {
      toast.error(error?.message || 'Failed to delete policy');
      setIsDeleting(false);
    }
  };

  if (loading || !policy) {
    return (
      <div className="container mx-auto p-6 max-w-2xl">
        <div className="mb-6">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate(ROUTES.POLICIES)}
            className="mb-2"
          >
            <ArrowLeft className="mr-2 h-4 w-4" />
            Back to Policies
          </Button>
          <Skeleton className="h-9 w-48" />
          <Skeleton className="h-4 w-64 mt-2" />
        </div>
        <div className="bg-card border rounded-lg p-6 space-y-4">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
        </div>
      </div>
    );
  }

  return (
    <div className="container mx-auto p-6 max-w-2xl">
      <div className="mb-6">
        <Button
          variant="ghost"
          size="sm"
          onClick={() => navigate(ROUTES.POLICIES)}
          className="mb-2"
        >
          <ArrowLeft className="mr-2 h-4 w-4" />
          Back to Policies
        </Button>
        <div className="flex items-start justify-between">
          <div>
            <h1 className="text-3xl font-bold">{policy.name}</h1>
            <p className="text-muted-foreground">Edit retention policy settings</p>
          </div>
          <Button
            onClick={() => setShowDeleteDialog(true)}
            variant="destructive"
            size="sm"
            disabled={linkedJobs.length > 0}
            title={linkedJobs.length > 0 ? 'Cannot delete: policy is in use by jobs' : undefined}
          >
            <Trash className="mr-2 h-4 w-4" />
            Delete
          </Button>
        </div>
      </div>

      <div className="space-y-6">
        <div className="bg-card border rounded-lg p-6">
          <PolicyForm
            defaultValues={policy}
            onSubmit={handleSubmit}
            onCancel={() => navigate(ROUTES.POLICIES)}
            isSubmitting={isSubmitting}
          />
        </div>

        {linkedJobs.length > 0 && (
          <Card>
            <CardHeader>
              <CardTitle>Used by Jobs</CardTitle>
              <CardDescription>
                This policy cannot be deleted while the following jobs are using it.
              </CardDescription>
            </CardHeader>
            <CardContent>
              <div className="flex flex-wrap gap-2">
                {linkedJobs.map((job) => (
                  <Badge
                    key={job.id}
                    variant="secondary"
                    className="cursor-pointer"
                    onClick={() => navigate(ROUTES.JOB_DETAIL(job.id))}
                  >
                    {job.name}
                  </Badge>
                ))}
              </div>
            </CardContent>
          </Card>
        )}
      </div>

      <DeleteDialog
        open={showDeleteDialog}
        onOpenChange={setShowDeleteDialog}
        title="Delete Policy"
        description={`Are you sure you want to delete "${policy.name}"? This action cannot be undone.`}
        onConfirm={handleDelete}
        isLoading={isDeleting}
      />
    </div>
  );
}
