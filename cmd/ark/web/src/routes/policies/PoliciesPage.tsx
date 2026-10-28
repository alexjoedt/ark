import { useState } from 'react';
import { useLoaderData, useNavigate, useSubmit } from 'react-router';
import type { ActionFunctionArgs } from 'react-router';
import { api } from '@/lib/api';
import { ROUTES } from '@/lib/constants';
import type { BackupPolicy } from '@/lib/types';
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
import { Plus, MoreVertical, Edit, Trash } from 'lucide-react';

export async function policiesLoader() {
  const policies = await api.listPolicies();
  return { policies };
}

export async function policiesAction({ request }: ActionFunctionArgs) {
  const formData = await request.formData();
  const intent = formData.get('intent') as string;
  const id = Number(formData.get('id'));

  try {
    switch (intent) {
      case 'delete':
        await api.deletePolicy(id);
        toast.success('Policy deleted');
        return { success: true };
      default:
        return { success: false };
    }
  } catch (error: any) {
    toast.error(error?.message || 'Operation failed');
    return { success: false, error: error?.message };
  }
}

export default function PoliciesPage() {
  const { policies } = useLoaderData<typeof policiesLoader>();
  const navigate = useNavigate();
  const submit = useSubmit();
  const [deletePolicy, setDeletePolicy] = useState<BackupPolicy | null>(null);
  const [isDeleting, setIsDeleting] = useState(false);

  const handleDelete = async () => {
    if (!deletePolicy) return;
    setIsDeleting(true);
    const formData = new FormData();
    formData.append('intent', 'delete');
    formData.append('id', deletePolicy.id.toString());
    submit(formData, { method: 'post' });
    setIsDeleting(false);
    setDeletePolicy(null);
  };

  const isEmpty = policies.length === 0;

  return (
    <Container className="flex flex-col h-[calc(100vh-4rem)] overflow-hidden">
      {/* Fixed Page Header - Never scrolls */}
      <PageHeader
        title="Retention Policies"
        description="Define rules for automatic cleanup of old backups"
        action={
          <Button onClick={() => navigate(ROUTES.POLICY_NEW)}>
            <Plus className="mr-2 h-4 w-4" />
            Create Policy
          </Button>
        }
        className="shrink-0"
      />

      {/* Main Content Card - Grows to fill space */}
      <Card className="flex flex-col flex-1 min-h-0">
        {/* Sticky Card Header - Fixed at top of card */}
        <CardHeader className="shrink-0 border-b">
          <CardTitle>Policies ({policies.length})</CardTitle>
        </CardHeader>

        {/* Scrollable Content Area */}
        <CardContent className="flex-1 overflow-auto p-0">
          {isEmpty ? (
            <div className="flex items-center justify-center h-full p-8">
              <EmptyState
                icon={<div className="text-4xl">🗂️</div>}
                title="No policies yet"
                description="Create a retention policy to automatically manage snapshots"
                action={{
                  label: 'Create Policy',
                  onClick: () => navigate(ROUTES.POLICY_NEW),
                }}
              />
            </div>
          ) : (
            <div className="relative">
              <Table>
                <TableHeader className="sticky top-0 bg-background z-10">
                  <TableRow>
                    <TableHead className="bg-background">Name</TableHead>
                    <TableHead className="bg-background">Description</TableHead>
                    <TableHead className="bg-background">Keep Count</TableHead>
                    <TableHead className="bg-background">Keep Days</TableHead>
                    <TableHead className="bg-background">Created</TableHead>
                    <TableHead className="text-right bg-background">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {policies.map((policy) => (
                    <TableRow
                      key={policy.id}
                      className="cursor-pointer"
                      onClick={() => navigate(ROUTES.POLICY_DETAIL(policy.id))}
                    >
                      <TableCell className="font-medium">{policy.name}</TableCell>
                      <TableCell className="text-muted-foreground">
                        {policy.description || '—'}
                      </TableCell>
                      <TableCell>
                        {policy.keep_count != null ? policy.keep_count : '—'}
                      </TableCell>
                      <TableCell>
                        {policy.keep_days != null ? `${policy.keep_days}d` : '—'}
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {formatDistanceToNow(new Date(policy.created_at), { addSuffix: true })}
                      </TableCell>
                      <TableCell className="text-right" onClick={(e) => e.stopPropagation()}>
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <Button variant="ghost" size="icon">
                              <MoreVertical className="h-4 w-4" />
                            </Button>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end">
                            <DropdownMenuItem
                              onClick={() => navigate(ROUTES.POLICY_DETAIL(policy.id))}
                            >
                              <Edit className="mr-2 h-4 w-4" />
                              Edit
                            </DropdownMenuItem>
                            <DropdownMenuItem
                              className="text-destructive"
                              onClick={() => setDeletePolicy(policy)}
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
        open={!!deletePolicy}
        onOpenChange={(open) => !open && setDeletePolicy(null)}
        title="Delete Policy"
        description={`Are you sure you want to delete "${deletePolicy?.name}"? Jobs using this policy will lose their retention rules.`}
        onConfirm={handleDelete}
        isLoading={isDeleting}
      />
    </Container>
  );
}
