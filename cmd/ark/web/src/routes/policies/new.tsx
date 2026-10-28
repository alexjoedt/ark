import { useState } from 'react';
import { useNavigate } from 'react-router';
import { api } from '@/lib/api';
import { ROUTES } from '@/lib/constants';
import type { CreatePolicyFormData } from '@/lib/schemas';
import { PolicyForm } from '@/components/features/policy-form';
import { toast } from 'sonner';
import { ArrowLeft } from 'lucide-react';
import { Button } from '@/components/ui/button';

export default function PolicyNewPage() {
  const navigate = useNavigate();
  const [isSubmitting, setIsSubmitting] = useState(false);

  const handleSubmit = async (data: CreatePolicyFormData) => {
    setIsSubmitting(true);
    try {
      await api.createPolicy(data);
      toast.success('Policy created successfully');
      navigate(ROUTES.POLICIES);
    } catch (error: any) {
      toast.error(error?.message || 'Failed to create policy');
      setIsSubmitting(false);
    }
  };

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
        <h1 className="text-3xl font-bold">Create Retention Policy</h1>
        <p className="text-muted-foreground">
          Define rules for automatic cleanup of old snapshots
        </p>
      </div>

      <div className="bg-card border rounded-lg p-6">
        <PolicyForm
          onSubmit={handleSubmit}
          onCancel={() => navigate(ROUTES.POLICIES)}
          isSubmitting={isSubmitting}
        />
      </div>
    </div>
  );
}
