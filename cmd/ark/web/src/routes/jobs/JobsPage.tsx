import { useState } from 'react';
import { useLoaderData, useNavigate, useSubmit } from 'react-router';
import type { ActionFunctionArgs } from 'react-router';
import { api } from '@/lib/api';
import { ROUTES } from '@/lib/constants';
import type { BackupJob } from '@/lib/types';
import { formatDistanceToNow } from 'date-fns';
import { toast } from 'sonner';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table';
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Switch } from '@/components/ui/switch';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu';
import { DeleteDialog } from '@/components/features/delete-dialog';
import { EmptyState } from '@/components/features/empty-state';
import { Container } from '@/components/layout/container';
import { PageHeader } from '@/components/layout/page-header';
import { Plus, MoreVertical, Play, Edit, Trash } from 'lucide-react';
import { useDocker } from '@/lib/contexts/docker-context';

export async function jobsLoader() {
  const jobs = await api.listJobs();
  return { jobs };
}

export async function jobsAction({ request }: ActionFunctionArgs) {
  const formData = await request.formData();
  const intent = formData.get('intent') as string;
  const id = Number(formData.get('id'));

  try {
    switch (intent) {
      case 'delete':
        await api.deleteJob(id);
        toast.success('Job deleted');
        return { success: true };
      case 'run':
        await api.runJobNow(id);
        toast.success('Job started');
        return { success: true };
      case 'enable':
        await api.toggleJob(id, true);
        toast.success('Job enabled');
        return { success: true };
      case 'disable':
        await api.toggleJob(id, false);
        toast.success('Job disabled');
        return { success: true };
      default:
        return { success: false };
    }
  } catch (error: any) {
    toast.error(error?.message || 'Operation failed');
    return { success: false, error: error?.message };
  }
}

export default function JobsPage() {
  const { jobs } = useLoaderData<typeof jobsLoader>();
  const navigate = useNavigate();
  const submit = useSubmit();
  const { dockerAvailable } = useDocker();
  const [deleteJob, setDeleteJob] = useState<BackupJob | null>(null);
  const [isDeleting, setIsDeleting] = useState(false);

  const handleToggle = (job: BackupJob) => {
    const formData = new FormData();
    formData.append('intent', job.enabled ? 'disable' : 'enable');
    formData.append('id', job.id.toString());
    submit(formData, { method: 'post' });
  };

  const handleRun = (job: BackupJob) => {
    const formData = new FormData();
    formData.append('intent', 'run');
    formData.append('id', job.id.toString());
    submit(formData, { method: 'post' });
  };

  const handleDelete = async () => {
    if (!deleteJob) return;
    setIsDeleting(true);
    const formData = new FormData();
    formData.append('intent', 'delete');
    formData.append('id', deleteJob.id.toString());
    submit(formData, { method: 'post' });
    setIsDeleting(false);
    setDeleteJob(null);
  };

  const isEmpty = jobs.length === 0;

  return (
    <Container className="flex flex-col h-[calc(100vh-4rem)] overflow-hidden">
      {/* Fixed Page Header - Never scrolls */}
      <PageHeader
        title="Backup Jobs"
        description="Schedule and manage automated backups"
        action={
          <Button
            onClick={() => navigate(ROUTES.JOB_NEW)}
            disabled={!dockerAvailable}
            title={!dockerAvailable ? 'Docker is unavailable' : undefined}
          >
            <Plus className="mr-2 h-4 w-4" />
            Create Job
          </Button>
        }
        className="flex-shrink-0"
      />

      {/* Main Content Card - Grows to fill space */}
      <Card className="flex flex-col flex-1 min-h-0">
        {/* Sticky Card Header - Fixed at top of card */}
        <CardHeader className="flex-shrink-0 border-b">
          <CardTitle>Jobs ({jobs.length})</CardTitle>
        </CardHeader>

        {/* Scrollable Content Area */}
        <CardContent className="flex-1 overflow-auto p-0">
          {isEmpty ? (
            <div className="flex items-center justify-center h-full p-8">
              <EmptyState
                icon={<div className="text-4xl">📅</div>}
                title="No jobs yet"
                description="Create your first scheduled backup job"
                action={{
                  label: 'Create Job',
                  onClick: () => navigate(ROUTES.JOB_NEW),
                  disabled: !dockerAvailable,
                  title: !dockerAvailable ? 'Docker is unavailable' : undefined,
                }}
              />
            </div>
          ) : (
            <div className="relative">
              <Table>
                <TableHeader className="sticky top-0 bg-background z-10">
                  <TableRow>
                    <TableHead className="bg-background">Name</TableHead>
                    <TableHead className="bg-background">Volume</TableHead>
                    <TableHead className="bg-background">Schedule</TableHead>
                    <TableHead className="bg-background">Policy</TableHead>
                    <TableHead className="bg-background">Enabled</TableHead>
                    <TableHead className="bg-background">Next Run</TableHead>
                    <TableHead className="bg-background">Last Run</TableHead>
                    <TableHead className="text-right bg-background">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {jobs.map((job) => (
                    <TableRow key={job.id}>
                      <TableCell>
                        <button
                          onClick={() => navigate(ROUTES.JOB_DETAIL(job.id))}
                          className="font-medium hover:underline text-left"
                        >
                          {job.name}
                        </button>
                      </TableCell>
                      <TableCell>
                        <Badge variant="outline">{job.volume_name}</Badge>
                      </TableCell>
                      <TableCell>
                        <code className="text-xs bg-muted px-2 py-1 rounded">{job.schedule}</code>
                      </TableCell>
                      <TableCell>
                        {job.policy_id ? (
                          <button
                            onClick={() => navigate(ROUTES.POLICY_DETAIL(job.policy_id!))}
                            className="text-sm text-muted-foreground hover:underline"
                          >
                            {job.policy?.name || `Policy #${job.policy_id}`}
                          </button>
                        ) : (
                          <span className="text-sm text-muted-foreground">None</span>
                        )}
                      </TableCell>
                      <TableCell>
                        <Switch
                          checked={job.enabled}
                          onCheckedChange={() => handleToggle(job)}
                        />
                      </TableCell>
                      <TableCell>
                        {job.next_run ? (
                          <span className="text-sm">
                            {formatDistanceToNow(new Date(job.next_run), { addSuffix: true })}
                          </span>
                        ) : (
                          <span className="text-sm text-muted-foreground">—</span>
                        )}
                      </TableCell>
                      <TableCell>
                        {job.last_run ? (
                          <span className="text-sm">
                            {formatDistanceToNow(new Date(job.last_run), { addSuffix: true })}
                          </span>
                        ) : (
                          <span className="text-sm text-muted-foreground">Never</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right">
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <Button variant="ghost" size="sm">
                              <MoreVertical className="h-4 w-4" />
                            </Button>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end">
                            <DropdownMenuItem onClick={() => handleRun(job)}>
                              <Play className="mr-2 h-4 w-4" />
                              Run Now
                            </DropdownMenuItem>
                            <DropdownMenuItem onClick={() => navigate(ROUTES.JOB_DETAIL(job.id))}>
                              <Edit className="mr-2 h-4 w-4" />
                              Edit
                            </DropdownMenuItem>
                            <DropdownMenuItem
                              onClick={() => setDeleteJob(job)}
                              className="text-destructive"
                            >
                              <Trash className="mr-2 h-4 w-4" />
                              Delete
                            </DropdownMenuItem>
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>

      <DeleteDialog
        open={!!deleteJob}
        onOpenChange={(open) => !open && setDeleteJob(null)}
        title="Delete Job?"
        description={`Are you sure you want to delete "${deleteJob?.name}"? This action cannot be undone.`}
        onConfirm={handleDelete}
        isLoading={isDeleting}
      />
    </Container>
  );
}
