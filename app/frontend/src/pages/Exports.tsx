// M11 Exports page: finished/in-progress clip exports, with download links
// for download-target exports and the share destination path for
// share-target ones.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Download, FolderOutput, Loader2, Trash2 } from 'lucide-react'
import { deleteExport, exportDownloadUrl, getExports, type Export } from '../lib/exportsApi'
import { formatBytes, formatDateLong } from '../lib/format'
import CopiesBadge from '../components/CopiesBadge'

function statusLabel(exp: Export): string {
  if (exp.path) return exp.share_id != null ? 'On share' : 'Ready'
  return 'Processing'
}

function statusStyle(exp: Export): string {
  if (!exp.path) return 'bg-surface-2 text-text-muted'
  return exp.share_id != null ? 'bg-cyan-500/10 text-cyan-400' : 'bg-success/10 text-success'
}

export default function Exports() {
  const queryClient = useQueryClient()
  const {
    data: exports,
    isLoading,
    isError,
  } = useQuery({ queryKey: ['exports'], queryFn: getExports, refetchInterval: 5000 })

  const deleteMutation = useMutation({
    mutationFn: (id: number) => deleteExport(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['exports'] }),
  })

  return (
    <div className="mx-auto flex max-w-4xl flex-col gap-6 px-8 py-8">
      <header className="flex items-center gap-2">
        <FolderOutput className="size-5 text-accent" />
        <h1 className="text-lg font-semibold text-text">Exports</h1>
      </header>

      {isLoading && (
        <div className="flex items-center justify-center py-16 text-text-muted">
          <Loader2 className="size-5 animate-spin" />
        </div>
      )}
      {isError && <p className="text-[13px] text-danger">Couldn&apos;t load exports.</p>}

      {exports && exports.length === 0 && (
        <p className="text-[13px] text-text-muted">
          No exports yet — open a trip and use the export bin to stitch a clip.
        </p>
      )}

      {exports && exports.length > 0 && (
        <div className="overflow-hidden rounded-lg border border-border">
          <table className="w-full text-left text-[13px]">
            <thead className="bg-surface/60 text-xs uppercase tracking-wide text-text-muted">
              <tr>
                <th className="px-3 py-2 font-medium">Name</th>
                <th className="px-3 py-2 font-medium">Created</th>
                <th className="px-3 py-2 font-medium">Size</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 font-medium">Copies</th>
                <th className="px-3 py-2 font-medium" />
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {exports.map((exp) => (
                <tr key={exp.id} className="text-text-secondary">
                  <td className="px-3 py-2 font-medium text-text">{exp.name}</td>
                  <td className="px-3 py-2 tabular-nums text-text-muted">{formatDateLong(exp.created_at)}</td>
                  <td className="px-3 py-2 tabular-nums text-text-muted">{exp.bytes > 0 ? formatBytes(exp.bytes) : '—'}</td>
                  <td className="px-3 py-2">
                    <span className={['rounded px-2 py-0.5 text-xs font-medium', statusStyle(exp)].join(' ')}>
                      {statusLabel(exp)}
                    </span>
                    {exp.share_id != null && exp.path && (
                      <span className="ml-2 font-mono text-xs text-text-muted">{exp.path}</span>
                    )}
                    {exp.expires_at && <span className="ml-2 text-xs text-text-muted">Expires {formatDateLong(exp.expires_at)}</span>}
                  </td>
                  <td className="px-3 py-2">
                    {exp.copies.length > 0 ? <CopiesBadge copies={exp.copies} /> : <span className="text-text-muted">—</span>}
                  </td>
                  <td className="px-3 py-2 text-right">
                    <div className="flex items-center justify-end gap-2">
                      {exp.path && exp.share_id == null && (
                        <a
                          href={exportDownloadUrl(exp.id)}
                          className="flex items-center gap-1 rounded border border-border px-2 py-1 text-text-secondary hover:border-text-muted"
                        >
                          <Download className="size-3.5" /> Download
                        </a>
                      )}
                      <button
                        type="button"
                        onClick={() => deleteMutation.mutate(exp.id)}
                        className="text-text-muted hover:text-danger"
                      >
                        <Trash2 className="size-3.5" />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
