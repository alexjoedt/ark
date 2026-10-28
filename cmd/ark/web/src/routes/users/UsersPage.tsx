import { useState } from 'react';
import { useLoaderData, useNavigate, redirect } from 'react-router';
import { api } from '@/lib/api';
import { ROUTES } from '@/lib/constants';
import type { User } from '@/lib/types';
import { format, formatDistanceToNow } from 'date-fns';
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
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
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
import { Plus, MoreVertical, Trash, KeyRound } from 'lucide-react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { changePasswordSchema, type ChangePasswordFormData } from '@/lib/schemas';

export async function usersLoader() {
  // Verify the caller is an authenticated admin before loading user data.
  try {
    const me = await api.getMe();
    if (me.role !== 'admin') {
      throw new Response('Forbidden', { status: 403 });
    }
  } catch (err) {
    if (err instanceof Response) throw err;
    return redirect(ROUTES.LOGIN);
  }

  const users = await api.listUsers();
  return { users };
}

export default function UsersPage() {
  const { users: initialUsers } = useLoaderData<typeof usersLoader>();
  const [users, setUsers] = useState<User[]>(initialUsers);
  const navigate = useNavigate();

  const [deleteUser, setDeleteUser] = useState<User | null>(null);
  const [isDeleting, setIsDeleting] = useState(false);
  const [passwordUser, setPasswordUser] = useState<User | null>(null);
  const [isChangingPassword, setIsChangingPassword] = useState(false);

  const {
    register,
    handleSubmit,
    reset,
    formState: { errors },
  } = useForm<ChangePasswordFormData>({
    resolver: zodResolver(changePasswordSchema),
  });

  const handleDelete = async () => {
    if (!deleteUser) return;
    setIsDeleting(true);
    try {
      await api.deleteUser(deleteUser.id);
      toast.success('User deleted');
      setUsers((prev) => prev.filter((u) => u.id !== deleteUser.id));
      setDeleteUser(null);
    } catch (error: any) {
      toast.error(error?.message || 'Failed to delete user');
    } finally {
      setIsDeleting(false);
    }
  };

  const handleChangePassword = async (data: ChangePasswordFormData) => {
    if (!passwordUser) return;
    setIsChangingPassword(true);
    try {
      await api.updatePassword(passwordUser.id, data.password);
      toast.success('Password updated');
      setPasswordUser(null);
      reset();
    } catch (error: any) {
      toast.error(error?.message || 'Failed to update password');
    } finally {
      setIsChangingPassword(false);
    }
  };

  const isEmpty = users.length === 0;

  return (
    <Container>
      <PageHeader
        title="Users"
        description="Manage arkUI user accounts and permissions"
      />

      <Card>
        <CardHeader className="border-b">
          <div className="flex items-center justify-between">
            <CardTitle>Users ({users.length})</CardTitle>
            <Button onClick={() => navigate(ROUTES.USER_NEW)}>
              <Plus className="h-4 w-4 mr-2" />
              New User
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          {isEmpty ? (
            <div className="flex items-center justify-center p-12">
              <EmptyState
                icon={<KeyRound className="h-12 w-12" />}
                title="No users"
                description="Create the first user to get started"
              />
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Username</TableHead>
                  <TableHead>Email</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {users.map((user) => (
                  <TableRow key={user.id}>
                    <TableCell className="font-medium">{user.username}</TableCell>
                    <TableCell className="text-muted-foreground">{user.email}</TableCell>
                    <TableCell>
                      <Badge variant={user.role === 'admin' ? 'default' : 'secondary'}>
                        {user.role}
                      </Badge>
                    </TableCell>
                    <TableCell>
                      <div className="flex flex-col gap-0.5">
                        <span className="text-sm">
                          {format(new Date(user.created_at), 'PPp')}
                        </span>
                        <span className="text-xs text-muted-foreground">
                          {formatDistanceToNow(new Date(user.created_at), { addSuffix: true })}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className="text-right">
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button variant="ghost" size="sm">
                            <MoreVertical className="h-4 w-4" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => setPasswordUser(user)}>
                            <KeyRound className="mr-2 h-4 w-4" />
                            Change Password
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            onClick={() => setDeleteUser(user)}
                            className="text-destructive focus:text-destructive"
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
          )}
        </CardContent>
      </Card>

      {/* Delete dialog */}
      <DeleteDialog
        title="Delete User?"
        description={
          deleteUser
            ? `Are you sure you want to delete "${deleteUser.username}"? This action cannot be undone.`
            : ''
        }
        onConfirm={handleDelete}
        open={!!deleteUser}
        onOpenChange={(open) => !open && setDeleteUser(null)}
        isLoading={isDeleting}
      />

      {/* Change password dialog */}
      <Dialog
        open={!!passwordUser}
        onOpenChange={(open) => {
          if (!open) {
            setPasswordUser(null);
            reset();
          }
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Change Password</DialogTitle>
            <DialogDescription>
              Set a new password for{' '}
              <span className="font-medium">{passwordUser?.username}</span>.
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleSubmit(handleChangePassword)} className="space-y-4">
            <div>
              <Label htmlFor="password">New Password</Label>
              <Input
                id="password"
                type="password"
                {...register('password')}
                disabled={isChangingPassword}
                autoComplete="new-password"
              />
              {errors.password && (
                <p className="text-sm text-destructive mt-1">{errors.password.message}</p>
              )}
            </div>
            <div>
              <Label htmlFor="confirmPassword">Confirm Password</Label>
              <Input
                id="confirmPassword"
                type="password"
                {...register('confirmPassword')}
                disabled={isChangingPassword}
                autoComplete="new-password"
              />
              {errors.confirmPassword && (
                <p className="text-sm text-destructive mt-1">{errors.confirmPassword.message}</p>
              )}
            </div>
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                onClick={() => {
                  setPasswordUser(null);
                  reset();
                }}
                disabled={isChangingPassword}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={isChangingPassword}>
                {isChangingPassword ? 'Saving…' : 'Save Password'}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </Container>
  );
}
