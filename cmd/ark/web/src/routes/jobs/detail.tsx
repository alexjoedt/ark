import { useState } from 'react';
import { useNavigate, useSubmit, useParams } from 'react-router';
import { api } from '@/lib/api';
import { useAsyncData } from '@/hooks/use-async-data';
import { useDocker } from '@/lib/contexts/docker-context';
import { ROUTES } from '@/lib/constants';
import type { CreateJobFormData } from '@/lib/schemas';
import { JobForm } from '@/components/features/job-form';
import { StatusBadge } from '@/components/features/status-badge';
import { DeleteDialog } from '@/components/features/delete-dialog';
import { toast } from 'sonner';
import { formatDistanceToNow, formatDuration, intervalToDuration } from 'date-fns';
import { ArrowLeft, Play, Trash } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import { EmptyState } from '@/components/features/empty-state';
import { Skeleton } from '@/components/ui/skeleton';

export default function JobDetailPage() {
  const params = useParams();
  const id = Number(params.id);

  const { data: job, loading: jobLoading } = useAsyncData(
    () => api.getJob(id),
    null as any,
    String(id)
  );
  const { data: history, loading: historyLoading } = useAsyncData(
    () => api.getJobHistory(id),
    [],
    String(id)
  );
  const { dockerAvailable, loading: dockerLoading } = useDocker();
  const { data: volumes, loading: volumesLoading } = useAsyncData(
    () => api.listVolumes(),
    [],
    String(id),
    { enabled: !dockerLoading && dockerAvailable }
  );
  const { data: policies, loading: policiesLoading } = useAsyncData(
    () => api.listPolicies(),
    [],
    String(id)
  );
  const loading = dockerLoading || jobLoading || historyLoading || volumesLoading || policiesLoading;

  const navigate = useNavigate();
  const submit = useSubmit();
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [showDeleteDialog, setShowDeleteDialog] = useState(false);
  const [isDeleting, setIsDeleting] = useState(false);

  const handleSubmit = async (data: CreateJobFormData) => {
    setIsSubmitting(true);
    try {
      await api.updateJob(job.id, data);
      toast.success('Job updated successfully');
      // Reload the page to show updated data
      window.location.reload();
    } catch (error: any) {
      toast.error(error?.message || 'Failed to update job');
      setIsSubmitting(false);
    }
  };

  const handleRunNow = () => {
    if (!job) return;
    const formData = new FormData();
    formData.append('intent', 'run');
    formData.append('id', job.id.toString());
    submit(formData, { method: 'post', action: ROUTES.JOBS });
  };

  const handleDelete = async () => {
    if (!job) return;
    setIsDeleting(true);
    try {
      await api.deleteJob(job.id);
      toast.success('Job deleted');
      navigate(ROUTES.JOBS);
    } catch (error: any) {
      toast.error(error?.message || 'Failed to delete job');
      setIsDeleting(false);
    }
  };

  if (loading || !job) {
    return (
      <div className="container mx-auto p-6 max-w-4xl">
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
    <div className="container mx-auto p-6 max-w-4xl">
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
        <div className="flex items-start justify-between">
          <div>
            <h1 className="text-3xl font-bold">{job.name}</h1>
            <p className="text-muted-foreground">Manage job settings and view execution history</p>
          </div>
          <div className="flex gap-2">
            <Button onClick={handleRunNow} variant="outline" size="sm">
              <Play className="mr-2 h-4 w-4" />
              Run Now
            </Button>
            <Button
              onClick={() => setShowDeleteDialog(true)}
              variant="destructive"
              size="sm"
            >
              <Trash className="mr-2 h-4 w-4" />
              Delete
            </Button>
          </div>
        </div>
      </div>

      <Tabs defaultValue="details" className="space-y-6">
        <TabsList>
          <TabsTrigger value="details">Details</TabsTrigger>
          <TabsTrigger value="history">
            History ({history.length})
          </TabsTrigger>
        </TabsList>

        <TabsContent value="details">
          <div className="bg-card border rounded-lg p-6">
            <JobForm
              defaultValues={job}
              volumes={volumes}
              policies={policies}
              onSubmit={handleSubmit}
              onCancel={() => navigate(ROUTES.JOBS)}
              isSubmitting={isSubmitting}
            />
          </div>
        </TabsContent>

        <TabsContent value="history">
          {history.length === 0 ? (
            <EmptyState
              icon={<div className="text-4xl">📊</div>}
              title="No execution history"
              description="This job hasn't run yet. Click 'Run Now' to execute it manually."
            />
          ) : (
            <div className="bg-card border rounded-lg">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Started</TableHead>
                    <TableHead>Completed</TableHead>
                    <TableHead>Duration</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Snapshot</TableHead>
                    <TableHead>Error</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {history.map((entry) => {
                    const duration = entry.completed_at
                      ? formatDuration(
                          intervalToDuration({ start: new Date(entry.started_at), end: new Date(entry.completed_at) }),
                          { format: ['minutes', 'seconds'] }
                        )
                      : '—';

                    return (
                      <TableRow key={entry.id}>
                        <TableCell>
                          <span className="text-sm">
                            {formatDistanceToNow(new Date(entry.started_at), { addSuffix: true })}
                          </span>
                        </TableCell>
                        <TableCell>
                          {entry.completed_at ? (
                            <span className="text-sm">
                              {formatDistanceToNow(new Date(entry.completed_at), { addSuffix: true })}
                            </span>
                          ) : (
                            <span className="text-sm text-muted-foreground">—</span>
                          )}
                        </TableCell>
                        <TableCell>
                          <span className="text-sm">{duration}</span>
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={entry.status} />
                        </TableCell>
                        <TableCell>
                          {entry.snapshot_id ? (
                            <code className="text-xs bg-muted px-2 py-1 rounded">
                              {entry.snapshot_id}
                            </code>
                          ) : (
                            <span className="text-sm text-muted-foreground">—</span>
                          )}
                        </TableCell>
                        <TableCell>
                          {entry.error_message ? (
                            <span className="text-sm text-destructive">{entry.error_message}</span>
                          ) : (
                            <span className="text-sm text-muted-foreground">—</span>
                          )}
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          )}
        </TabsContent>
      </Tabs>

      <DeleteDialog
        open={showDeleteDialog}
        onOpenChange={setShowDeleteDialog}
        title="Delete Job?"
        description={`Are you sure you want to delete "${job.name}"? This action cannot be undone.`}
        onConfirm={handleDelete}
        isLoading={isDeleting}
      />
    </div>
  );
}
