import { useEffect, useState } from 'react'
import { api, type Commit, type GitFile, type GitStatus } from './api'

export const statusLetter: Record<GitFile['status'], string> = {
  modified: 'M',
  added: 'A',
  deleted: 'D',
  renamed: 'R',
  untracked: 'U',
}

// SourceControl: changed files and the commit box (sidebar).
export function SourceControl(props: {
  project: string
  status: GitStatus | null
  onChanged: () => void
  onOpenDiff: (path: string) => void
  onError: (e: string) => void
  confirm: (label: string, title: string, onConfirm: () => void) => React.ReactNode
}) {
  const { project, status, onChanged, onOpenDiff, onError, confirm } = props
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)

  if (!status) return null
  if (!status.repo) {
    return (
      <>
        <div className="gha-section-title">Version control</div>
        <div className="gha-git-init">
          <button className="gha-small" onClick={() => api.git.init(project).then(onChanged, (e) => onError(String(e)))}>
            Initialize git
          </button>
        </div>
      </>
    )
  }
  const files = status.files ?? []
  const commit = async () => {
    if (!message.trim() || files.length === 0) return
    setBusy(true)
    try {
      await api.git.commit(project, message.trim())
      setMessage('')
      onChanged()
    } catch (e) {
      onError(String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <div className="gha-section-title">
        Changes <span className="gha-muted">{files.length || ''}</span>
        <span className="gha-branch" title={`HEAD ${status.head ?? ''}`}>⎇ {status.branch || 'main'}</span>
      </div>
      {files.length > 0 && (
        <div className="gha-commit-box">
          <textarea
            placeholder="Commit message (Ctrl+Enter)"
            value={message}
            rows={2}
            onChange={(e) => setMessage(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) commit()
            }}
          />
          <button className="gha-small" disabled={busy || !message.trim()} onClick={commit}>
            Commit {files.length} file{files.length === 1 ? '' : 's'}
          </button>
        </div>
      )}
      <ul className="gha-list gha-files">
        {files.map((f) => (
          <li key={f.path} onClick={() => onOpenDiff(f.path)} title={`${f.status}: show diff`}>
            <span className="gha-file-name">{f.path}</span>
            <span className={`gha-git-badge gha-git-${f.status}`}>{statusLetter[f.status]}</span>
            {confirm('↺', 'Discard changes (cannot be undone)', () =>
              api.git.discard(project, f.path).then(onChanged, (e) => onError(String(e))),
            )}
          </li>
        ))}
        {files.length === 0 && <li className="gha-muted gha-static">No uncommitted changes</li>}
      </ul>
    </>
  )
}

// History: commits of the project; expand a commit to see and diff its files.
export function History(props: {
  project: string
  head: string | undefined // reload when it changes
  onOpenCommitDiff: (path: string, commit: Commit, status: GitFile['status']) => void
  onError: (e: string) => void
}) {
  const { project, head, onOpenCommitDiff, onError } = props
  const [commits, setCommits] = useState<Commit[]>([])
  const [open, setOpen] = useState<string | null>(null)
  const [files, setFiles] = useState<GitFile[]>([])

  useEffect(() => {
    api.git.log(project).then(setCommits, (e) => onError(String(e)))
  }, [project, head, onError])

  const toggle = async (c: Commit) => {
    if (open === c.hash) return setOpen(null)
    setOpen(c.hash)
    setFiles([])
    try {
      setFiles(await api.git.commitFiles(project, c.hash))
    } catch (e) {
      onError(String(e))
    }
  }

  return (
    <div className="gha-history">
      {commits.length === 0 && <div className="gha-empty">No commits yet.</div>}
      <ul className="gha-commits">
        {commits.map((c) => (
          <li key={c.hash}>
            <div className={`gha-commit${open === c.hash ? ' gha-active' : ''}`} onClick={() => toggle(c)}>
              <span className="gha-commit-hash">{c.short}</span>
              <span className="gha-commit-msg">{c.message}</span>
              <span className="gha-commit-meta">
                {c.author} · {relativeTime(c.date)}
              </span>
            </div>
            {open === c.hash && (
              <ul className="gha-commit-files">
                {files.map((f) => (
                  <li key={f.path} onClick={() => onOpenCommitDiff(f.path, c, f.status)}>
                    <span className={`gha-git-badge gha-git-${f.status}`}>{statusLetter[f.status]}</span>
                    {f.path}
                  </li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ul>
    </div>
  )
}

function relativeTime(iso: string) {
  const s = (Date.now() - new Date(iso).getTime()) / 1000
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  if (s < 30 * 86400) return `${Math.floor(s / 86400)} d ago`
  return new Date(iso).toLocaleDateString()
}
