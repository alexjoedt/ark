import { useNavigate } from "react-router";
import { useState, useMemo } from "react";
import { api } from "@/lib/api";
import { ROUTES } from "@/lib/constants";
import { formatDistanceToNow } from "date-fns";
import type { Volume, Snapshot } from "@/lib/types";
import { useAsyncData } from "@/hooks/use-async-data";
import {
  Card,
  CardHeader,
  CardTitle,
  CardContent,
} from "@/components/ui/custom/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Container } from "@/components/layout/container";
import { PageHeader } from "@/components/layout/page-header";
import { BackupDialog } from "@/components/features/backup-dialog";
import {
  Database,
  Plus,
  Archive,
  Clock,
  HardDrive,
  AlertCircle,
  CheckCircle2,
  Search,
  X,
} from "lucide-react";
import { DataTable, type ColumnDef } from "@/components/table";
import { useDocker } from "@/lib/contexts/docker-context";

interface VolumeWithSnapshots extends Volume {
  snapshot_count: number;
  latest_backup?: string;
}

function enrichVolumes(volumes: Volume[], snapshots: Snapshot[]): VolumeWithSnapshots[] {
  const volumeMap = new Map<string, { count: number; latest?: string }>();

  snapshots.forEach((snapshot) => {
    const existing = volumeMap.get(snapshot.volume) || { count: 0 };
    existing.count++;
    if (
      !existing.latest ||
      new Date(snapshot.created_at) > new Date(existing.latest)
    ) {
      existing.latest = snapshot.created_at;
    }
    volumeMap.set(snapshot.volume, existing);
  });

  return volumes
    .filter((volume) => volume.name != null)
    .map((volume) => {
      const snapshotInfo = volumeMap.get(volume.name) || { count: 0 };
      return {
        ...volume,
        snapshot_count: snapshotInfo.count,
        latest_backup: snapshotInfo.latest,
      };
    });
}

// Helper to determine backup health status
function getBackupStatus(volume: VolumeWithSnapshots): {
  label: string;
  variant: "default" | "secondary" | "destructive" | "outline";
  icon: React.ReactNode;
} {
  if (volume.snapshot_count === 0) {
    return {
      label: "No backups",
      variant: "secondary",
      icon: <AlertCircle className="h-3 w-3" />,
    };
  }

  if (!volume.latest_backup) {
    return {
      label: "No backups",
      variant: "secondary",
      icon: <AlertCircle className="h-3 w-3" />,
    };
  }

  const daysSinceBackup = Math.floor(
    (Date.now() - new Date(volume.latest_backup).getTime()) /
      (1000 * 60 * 60 * 24)
  );

  if (daysSinceBackup <= 7) {
    return {
      label: "Healthy",
      variant: "default",
      icon: <CheckCircle2 className="h-3 w-3" />,
    };
  }

  if (daysSinceBackup <= 30) {
    return {
      label: "Aging",
      variant: "outline",
      icon: <Clock className="h-3 w-3" />,
    };
  }

  return {
    label: "Outdated",
    variant: "destructive",
    icon: <AlertCircle className="h-3 w-3" />,
  };
}

export default function VolumesPage() {
  const { dockerAvailable, loading: dockerLoading } = useDocker();
  const { data: rawVolumes, loading: volumesLoading } = useAsyncData(
    () => api.listVolumes(),
    [],
    undefined,
    { enabled: !dockerLoading && dockerAvailable }
  );
  const { data: snapshots, loading: snapshotsLoading } = useAsyncData(
    () => api.listSnapshots(),
    []
  );
  const loading = dockerLoading || volumesLoading || snapshotsLoading;
  const volumes = useMemo(
    () => enrichVolumes(rawVolumes, snapshots),
    [rawVolumes, snapshots]
  );

  const navigate = useNavigate();
  const [searchValue, setSearchValue] = useState("");
  const [backupDialogOpen, setBackupDialogOpen] = useState(false);
  const [selectedVolume, setSelectedVolume] = useState<string>("");

  // Filter volumes by search term
  const filteredVolumes = useMemo(
    () =>
      volumes.filter((volume) =>
        (volume.name ?? '').toLowerCase().includes(searchValue.toLowerCase())
      ),
    [volumes, searchValue]
  );

  // Calculate summary stats
  const stats = useMemo(() => {
    const totalSnapshots = volumes.reduce((sum, v) => sum + v.snapshot_count, 0);
    const volumesWithBackups = volumes.filter(
      (v) => v.snapshot_count > 0
    ).length;
    const recentBackups = volumes.filter((v) => {
      if (!v.latest_backup) return false;
      const daysSince =
        (Date.now() - new Date(v.latest_backup).getTime()) /
        (1000 * 60 * 60 * 24);
      return daysSince <= 7;
    }).length;

    return {
      totalVolumes: volumes.length,
      totalSnapshots,
      volumesWithBackups,
      recentBackups,
    };
  }, [volumes]);

  const handleBackupClick = (volume: VolumeWithSnapshots) => {
    setSelectedVolume(volume.name);
    setBackupDialogOpen(true);
  };

  const handleRowClick = (volume: VolumeWithSnapshots) => {
    navigate(`${ROUTES.VOLUMES}/${encodeURIComponent(volume.name)}`);
  };

  // Define columns for DataTable
  const columns = useMemo<ColumnDef<VolumeWithSnapshots>[]>(
    () => [
      {
        id: "name",
        header: "Volume Name",
        accessor: "name",
        width: "w-[40%]",
        searchable: true,
        cell: ({ row }) => (
          <div className="flex flex-col gap-1">
            <span className="text-sm font-medium">{row.name}</span>
            <span className="text-xs text-muted-foreground flex items-center gap-1.5">
              <code className="bg-muted px-1.5 py-0.5 rounded text-[10px]">
                {row.driver}
              </code>
              <span>•</span>
              <span>
                Created{" "}
                {formatDistanceToNow(new Date(row.created_at), {
                  addSuffix: true,
                })}
              </span>
            </span>
          </div>
        ),
      },
      {
        id: "status",
        header: "Status",
        cell: ({ row }) => {
          const status = getBackupStatus(row);
          return (
            <Badge variant={status.variant} className="gap-1.5">
              {status.icon}
              {status.label}
            </Badge>
          );
        },
      },
      {
        id: "snapshots",
        header: "Snapshots",
        accessor: "snapshot_count",
        cell: ({ row }) => (
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium">{row.snapshot_count}</span>
            {row.snapshot_count > 0 && (
              <span className="text-xs text-muted-foreground">
                backup{row.snapshot_count !== 1 ? "s" : ""}
              </span>
            )}
          </div>
        ),
      },
      {
        id: "latest_backup",
        header: "Latest Backup",
        accessor: "latest_backup",
        cell: ({ row }) =>
          row.latest_backup ? (
            <span className="text-sm">
              {formatDistanceToNow(new Date(row.latest_backup), {
                addSuffix: true,
              })}
            </span>
          ) : (
            <span className="text-sm text-muted-foreground">Never</span>
          ),
      },
    ],
    []
  );

  return (
    <Container className="flex flex-col h-[calc(100vh-4rem)] overflow-hidden">
      <PageHeader
        title="Docker Volumes"
        description="Manage backups for your Docker volumes"
        className="shrink-0"
      />

      {/* Summary Stats */}
      <div className="shrink-0 mb-4">
        <div className="grid gap-3 md:grid-cols-4">
          <Card size="sm">
            <CardContent>
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-xs text-muted-foreground">Total Volumes</p>
                  {loading ? (
                    <Skeleton className="h-7 w-10 mt-1" />
                  ) : (
                    <p className="text-xl font-semibold mt-1">
                      {stats.totalVolumes}
                    </p>
                  )}
                </div>
                <div className="h-8 w-8 rounded-md bg-primary/10 flex items-center justify-center">
                  <HardDrive className="h-4 w-4 text-primary" />
                </div>
              </div>
            </CardContent>
          </Card>

          <Card size="sm">
            <CardContent>
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-xs text-muted-foreground">With Backups</p>
                  {loading ? (
                    <Skeleton className="h-7 w-10 mt-1" />
                  ) : (
                    <p className="text-xl font-semibold mt-1">
                      {stats.volumesWithBackups}
                    </p>
                  )}
                </div>
                <div className="h-8 w-8 rounded-md bg-blue-500/10 flex items-center justify-center">
                  <Database className="h-4 w-4 text-blue-500" />
                </div>
              </div>
            </CardContent>
          </Card>

          <Card size="sm">
            <CardContent>
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-xs text-muted-foreground">
                    Total Snapshots
                  </p>
                  {loading ? (
                    <Skeleton className="h-7 w-10 mt-1" />
                  ) : (
                    <p className="text-xl font-semibold mt-1">
                      {stats.totalSnapshots}
                    </p>
                  )}
                </div>
                <div className="h-8 w-8 rounded-md bg-green-500/10 flex items-center justify-center">
                  <Archive className="h-4 w-4 text-green-500" />
                </div>
              </div>
            </CardContent>
          </Card>

          <Card size="sm">
            <CardContent>
              <div className="flex items-center justify-between">
                <div>
                  <p className="text-xs text-muted-foreground">
                    Recent Backups
                  </p>
                  {loading ? (
                    <Skeleton className="h-7 w-10 mt-1" />
                  ) : (
                    <>
                      <p className="text-xl font-semibold mt-1">
                        {stats.recentBackups}
                      </p>
                      <p className="text-[10px] text-muted-foreground mt-0.5">
                        Last 7 days
                      </p>
                    </>
                  )}
                </div>
                <div className="h-8 w-8 rounded-md bg-emerald-500/10 flex items-center justify-center">
                  <CheckCircle2 className="h-4 w-4 text-emerald-500" />
                </div>
              </div>
            </CardContent>
          </Card>
        </div>
      </div>

      <Card className="flex flex-col flex-1 min-h-0">
        <CardHeader className="flex-shrink-0 border-b py-3 space-y-3">
          <CardTitle className="text-base">All Volumes</CardTitle>
          <div className="relative">
            <Search className="absolute left-2 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
            <Input
              type="text"
              placeholder="Search volumes by name..."
              value={searchValue}
              onChange={(e) => setSearchValue(e.target.value)}
              className="pl-8 pr-8 h-9"
            />
            {searchValue && (
              <button
                onClick={() => setSearchValue('')}
                className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              >
                <X className="h-4 w-4" />
              </button>
            )}
          </div>
        </CardHeader>
        <CardContent className="flex-1 overflow-auto p-0">
          {loading ? (
            <div className="p-6 space-y-4">
              {[...Array(5)].map((_, i) => (
                <div key={i} className="flex items-center gap-4">
                  <Skeleton className="h-5 w-[40%]" />
                  <Skeleton className="h-5 w-[15%]" />
                  <Skeleton className="h-5 w-[10%]" />
                  <Skeleton className="h-5 w-[20%]" />
                </div>
              ))}
            </div>
          ) : (
            <DataTable
            data={filteredVolumes}
            columns={columns}
            rowIdAccessor={(row) => row.name}
            onRowClick={handleRowClick}
            actions={{
              items: [
                {
                  label: "Backup",
                  icon: <Plus className="h-3.5 w-3.5" />,
                  onClick: handleBackupClick,
                },
              ],
              position: "right",
            }}
            emptyState={{
              icon: <Database className="h-12 w-12" />,
              title: searchValue ? "No volumes found" : "No Docker volumes",
              description: searchValue
                ? "Try adjusting your search query"
                : "No Docker volumes found on this system",
            }}
          />
          )}
        </CardContent>
      </Card>

      <BackupDialog
        volumeName={selectedVolume}
        open={backupDialogOpen}
        onOpenChange={setBackupDialogOpen}
      />
    </Container>
  );
}
