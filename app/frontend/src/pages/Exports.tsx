// M11 Exports page: finished/in-progress clip exports, with download links
// for download-target exports and the share destination path for
// share-target ones.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Download, FolderOutput, Loader2, Trash2 } from 'lucide-react'
import { deleteExport, exportDownloadUrl, getExports, type Export } from '../lib/exportsApi'
import { formatBytes, formatDateLong } from '../lib/format'

function statusLabel(exp: Export): string {
  if (exp.path) return exp.share_id != null ? 'On share' : 'Ready'
  return 'Processing'
}

function statusStyle(exp: Export): string {
  if (!exp.path) return 'bg-neutral-800 text-neutral-400'
  return exp.share_id != null ? 'bg-cyan-500/10 text-cyan-400' : 'bg-emerald-500/10 text-emerald-400'
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
        <FolderOutput className="size-5 text-amber-500" />
        <h1 className="text-lg font-semibold text-neutral-100">Exports</h1>
      </header>

      {isLoading && (
        <div className="flex items-center justify-center py-16 text-neutral-400">
          <Loader2 className="size-5 animate-spin" />
        </div>
      )}
      {isError && <p className="text-[13px] text-red-400">Couldn&apos;t load exports.</p>}

      {exports && exports.length === 0 && (
        <p className="text-[13px] text-neutral-400">
          No exports yet — open a trip and use the export bin to stitch a clip.
        </p>
      )}

      {exports && exports.length > 0 && (
        <div className="overflow-hidden rounded-lg border border-neutral-800">
          <table className="w-full text-left text-[13px]">
            <thead className="bg-neutral-900/60 text-xs uppercase tracking-wide text-neutral-500">
              <tr>
                <th className="px-3 py-2 font-medium">Name</th>
                <th className="px-3 py-2 font-medium">Created</th>
                <th className="px-3 py-2 font-medium">Size</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 font-medium" />
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-900">
              {exports.map((exp) => (
                <tr key={exp.id} className="text-neutral-300">
                  <td className="px-3 py-2 font-medium text-neutral-100">{exp.name}</td>
                  <td className="px-3 py-2 tabular-nums text-neutral-500">{formatDateLong(exp.created_at)}</td>
                  <td className="px-3 py-2 tabular-nums text-neutral-500">{exp.bytes > 0 ? formatBytes(exp.bytes) : '—'}</td>
                  <td className="px-3 py-2">
                    <span className={['rounded px-2 py-0.5 text-xs font-medium', statusStyle(exp)].join(' ')}>
                      {statusLabel(exp)}
                    </span>
                    {exp.share_id != null && exp.path && (
                      <span className="ml-2 font-mono text-xs text-neutral-400">{exp.path}</span>
                    )}
                    {exp.expires_at && <span className="ml-2 text-xs text-neutral-400">Expires {formatDateLong(exp.expires_at)}</span>}
                  </td>
                  <td className="px-3 py-2 text-right">
                    <div className="flex items-center justify-end gap-2">
                      {exp.path && exp.share_id == null && (
                        <a
                          href={exportDownloadUrl(exp.id)}
                          className="flex items-center gap-1 rounded border border-neutral-800 px-2 py-1 text-neutral-300 hover:border-neutral-700"
                        >
                          <Download className="size-3.5" /> Download
                        </a>
                      )}
                      <button
                        type="button"
                        onClick={() => deleteMutation.mutate(exp.id)}
                        className="text-neutral-400 hover:text-red-400"
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
