import { useState, useEffect } from "react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { useWebSocket } from "@/lib/contexts/websocket-context";
import { useDocker } from "@/lib/contexts/docker-context";
import type { Snapshot, Volume, RestoreCompletedEvent, RestoreFailedEvent } from "@/lib/types";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { AlertCircle } from "lucide-react";

interface RestoreDialogProps {
  snapshot: Snapshot;
  volumes: Volume[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onRestoreComplete?: () => void;
}

export function RestoreDialog({
  snapshot,
  volumes,
  open,
  onOpenChange,
  onRestoreComplete,
}: RestoreDialogProps) {
  const { subscribe, unsubscribe } = useWebSocket();
  const { dockerAvailable } = useDocker();
  const [selectedVolume, setSelectedVolume] = useState<string>(snapshot.volume);
  const [force, setForce] = useState(false);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [isRestoring, setIsRestoring] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open || !isRestoring) return;

    const handleRestoreCompleted = (event: RestoreCompletedEvent) => {
      if (event.data.snapshot_id === snapshot.id && event.data.volume === selectedVolume) {
        setIsRestoring(false);
        setIsSubmitting(false);
        toast.success("Restore completed", {
          description: `Successfully restored snapshot ${snapshot.id} to ${selectedVolume}`,
        });
        onRestoreComplete?.();
        onOpenChange(false);
      }
    };

    const handleRestoreFailed = (event: RestoreFailedEvent) => {
      if (event.data.snapshot_id === snapshot.id && event.data.volume === selectedVolume) {
        setIsRestoring(false);
        setIsSubmitting(false);
        const errorMsg = event.data.error || "Restore failed";
        setError(errorMsg);
        toast.error("Restore failed", { description: errorMsg });
      }
    };

    subscribe("restore.completed", handleRestoreCompleted as any);
    subscribe("restore.failed", handleRestoreFailed as any);

    return () => {
      unsubscribe("restore.completed", handleRestoreCompleted as any);
      unsubscribe("restore.failed", handleRestoreFailed as any);
    };
  }, [open, isRestoring, snapshot.id, selectedVolume, subscribe, unsubscribe, onRestoreComplete, onOpenChange]);

  const handleSubmit = async () => {
    if (!dockerAvailable) {
      setError("Docker is not available. Cannot perform restore operation.");
      return;
    }

    if (!selectedVolume) {
      setError("Please select a target volume");
      return;
    }

    setError(null);
    setIsSubmitting(true);

    try {
      await api.restore(snapshot.id, selectedVolume, force);
      setIsRestoring(true);
      toast.info("Restore started", {
        description: `Restoring snapshot ${snapshot.id} to ${selectedVolume}...`,
      });
    } catch (err: any) {
      console.error("Restore failed:", err);
      const errorMessage =
        err.response?.data?.error?.message || err.message || "Failed to start restore";
      setError(errorMessage);
      toast.error("Restore failed", { description: errorMessage });
      setIsSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Restore Snapshot</DialogTitle>
          <DialogDescription>
            {isRestoring ? (
              <>
                Restoring snapshot <strong>{snapshot.id}</strong> to{" "}
                <strong>{selectedVolume}</strong>. Please wait...
              </>
            ) : (
              <>
                Restore snapshot <strong>{snapshot.id}</strong> to a Docker volume. This
                will stop any containers using the target volume during the restore process.
              </>
            )}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4 py-4">
          <div className="space-y-2">
            <Label htmlFor="target-volume">Target Volume</Label>
            <Select
              value={selectedVolume}
              onValueChange={setSelectedVolume}
              disabled={isRestoring}
            >
              <SelectTrigger id="target-volume">
                <SelectValue placeholder="Select a volume" />
              </SelectTrigger>
              <SelectContent>
                {volumes.map((volume) => (
                  <SelectItem key={volume.name} value={volume.name}>
                    {volume.name}
                    {volume.name === snapshot.volume && " (original)"}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-xs text-muted-foreground">
              Select the volume to restore the snapshot into
            </p>
          </div>

          <div className="flex items-center space-x-2">
            <Checkbox
              id="force"
              checked={force}
              onCheckedChange={(checked) => setForce(checked === true)}
              disabled={isRestoring}
            />
            <Label
              htmlFor="force"
              className="text-sm font-normal cursor-pointer leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70"
            >
              Force restore (overwrite existing data)
            </Label>
          </div>

          {force && (
            <Alert variant="destructive">
              <AlertCircle className="h-4 w-4" />
              <AlertDescription>
                Warning: Force restore will completely replace all existing data in the
                target volume. This action cannot be undone.
              </AlertDescription>
            </Alert>
          )}

          {error && (
            <Alert variant="destructive">
              <AlertCircle className="h-4 w-4" />
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}

          {!dockerAvailable && !error && (
            <Alert variant="destructive">
              <AlertCircle className="h-4 w-4" />
              <AlertDescription>
                Docker is not available. Restore operations are disabled until Docker is
                accessible.
              </AlertDescription>
            </Alert>
          )}
        </div>

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={isSubmitting || isRestoring}
          >
            Cancel
          </Button>
          <Button
            onClick={handleSubmit}
            disabled={isSubmitting || isRestoring || !selectedVolume || !dockerAvailable}
          >
            {isRestoring ? "Restoring..." : isSubmitting ? "Starting..." : "Start Restore"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
