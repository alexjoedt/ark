import { useState, useRef } from 'react';
import type { ChangeEvent } from 'react';
import { toast } from 'sonner';
import { api } from '@/lib/api';
import {
    Dialog,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle
} from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Label } from '@/components/ui/label';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Progress } from '@/components/ui/progress';
import { AlertCircle, Upload, FileArchive, CheckCircle2 } from 'lucide-react';
import { formatBytes } from '@/lib/utils';

interface ImportSnapshotDialogProps {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    onImportSuccess?: () => void;
}

export function ImportSnapshotDialog({
    open,
    onOpenChange,
    onImportSuccess
}: ImportSnapshotDialogProps) {
    const [selectedFile, setSelectedFile] = useState<File | null>(null);
    const [isImporting, setIsImporting] = useState(false);
    const [importProgress, setImportProgress] = useState(0);
    const [error, setError] = useState<string | null>(null);
    const fileInputRef = useRef<HTMLInputElement>(null);

    const handleFileChange = (e: ChangeEvent<HTMLInputElement>) => {
        const file = e.target.files?.[0];
        if (!file) {
            setSelectedFile(null);
            return;
        }

        // Validate file extension
        if (!file.name.endsWith('.tar.gz')) {
            setError('Only .tar.gz files are allowed');
            setSelectedFile(null);
            return;
        }

        setError(null);
        setSelectedFile(file);
    };

    const handleImport = async () => {
        if (!selectedFile) {
            setError('Please select a file');
            return;
        }

        setError(null);
        setIsImporting(true);
        setImportProgress(0);

        try {
            const formData = new FormData();
            // Backend expects the field named "archive"
            formData.append('archive', selectedFile);

            await api.importSnapshot(formData, (progress) => {
                setImportProgress(Math.round(progress));
            });

            toast.success('Snapshot imported successfully', {
                description: `${selectedFile.name} has been imported and validated`
            });

            // Reset state
            setSelectedFile(null);
            setImportProgress(0);
            if (fileInputRef.current) {
                fileInputRef.current.value = '';
            }

            // Close dialog and trigger refresh
            onOpenChange(false);
            onImportSuccess?.();
        } catch (err: any) {
            console.error('Import error:', err);
            const errorMessage =
                err.response?.data?.error || err.message || 'Failed to import snapshot';
            setError(errorMessage);
            toast.error('Import failed', { description: errorMessage });
        } finally {
            setIsImporting(false);
        }
    };

    const handleDialogOpenChange = (open: boolean) => {
        // Don't allow closing while importing
        if (isImporting) return;

        // Reset state when closing
        if (!open) {
            setSelectedFile(null);
            setError(null);
            setImportProgress(0);
            if (fileInputRef.current) {
                fileInputRef.current.value = '';
            }
        }

        onOpenChange(open);
    };

    return (
        <Dialog open={open} onOpenChange={handleDialogOpenChange}>
            <DialogContent>
                <DialogHeader>
                    <DialogTitle>Import Snapshot</DialogTitle>
                    <DialogDescription>
                        Import a snapshot from a tar.gz file. The file will be validated to
                        ensure it contains valid ark metadata before being accepted.
                    </DialogDescription>
                </DialogHeader>

                <div className="space-y-4 py-4">
                    <div className="space-y-2">
                        <Label htmlFor="snapshot-file">Snapshot File</Label>
                        <div className="flex items-center gap-2">
                            <Button
                                type="button"
                                variant="outline"
                                onClick={() => fileInputRef.current?.click()}
                                disabled={isImporting}
                                className="w-full"
                            >
                                <Upload className="h-4 w-4 mr-2" />
                                {selectedFile ? 'Change File' : 'Select File'}
                            </Button>
                            <input
                                ref={fileInputRef}
                                id="snapshot-file"
                                type="file"
                                onChange={handleFileChange}
                                className="hidden"
                                disabled={isImporting}
                            />
                        </div>
                        <p className="text-xs text-muted-foreground">
                            Only .tar.gz files with valid ark metadata are accepted
                        </p>
                    </div>

                    {selectedFile && !isImporting && (
                        <Alert>
                            <FileArchive className="h-4 w-4" />
                            <AlertDescription>
                                <div className="flex items-center justify-between">
                                    <div>
                                        <div className="font-medium">{selectedFile.name}</div>
                                        <div className="text-xs text-muted-foreground mt-1">
                                            {formatBytes(selectedFile.size)}
                                        </div>
                                    </div>
                                    <CheckCircle2 className="h-5 w-5 text-green-600" />
                                </div>
                            </AlertDescription>
                        </Alert>
                    )}

                    {isImporting && (
                        <div className="space-y-2">
                            <div className="flex items-center justify-between text-sm">
                                <span>Importing...</span>
                                <span className="font-medium">{importProgress}%</span>
                            </div>
                            <Progress value={importProgress} />
                            <p className="text-xs text-muted-foreground">
                                Please wait while the snapshot is imported and validated
                            </p>
                        </div>
                    )}

                    {error && (
                        <Alert variant="destructive">
                            <AlertCircle className="h-4 w-4" />
                            <AlertDescription>{error}</AlertDescription>
                        </Alert>
                    )}
                </div>

                <DialogFooter>
                    <Button variant="outline" onClick={() => handleDialogOpenChange(false)} disabled={isImporting}>
                        Cancel
                    </Button>
                    <Button onClick={handleImport} disabled={isImporting || !selectedFile}>
                        {isImporting ? 'Importing...' : 'Import'}
                    </Button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}
