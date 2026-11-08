import { useEffect, useRef, useState } from 'react'
import { copyText } from './Entities'
import { CopyIcon, TrashIcon } from './Icons'

// DeleteProjectDialog asks the user to type the project name before deleting.
export function DeleteProjectDialog({ name, onCancel, onConfirm }: { name: string; onCancel: () => void; onConfirm: () => Promise<void> }) {
  const [typed, setTyped] = useState('')
  const [copied, setCopied] = useState(false)
  const [busy, setBusy] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  const ok = typed === name

  useEffect(() => input.current?.focus(), [])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onCancel()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onCancel])

  const confirm = async () => {
    if (!ok || busy) return
    setBusy(true)
    try {
      await onConfirm()
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="gha-modal-backdrop" onMouseDown={onCancel}>
      <div className="gha-modal" role="dialog" aria-modal="true" aria-labelledby="gha-del-title" onMouseDown={(e) => e.stopPropagation()}>
        <h2 id="gha-del-title">Delete project</h2>
        <p>
          This permanently deletes the project <strong>{name}</strong> with all its files and its git history. The running
          automation is stopped. This cannot be undone.
        </p>
        <p className="gha-modal-label">Type the project name to confirm:</p>
        <div className="gha-copy-name">
          <code>{name}</code>
          <button
            className="gha-ghost gha-small gha-icon-btn"
            onClick={() => {
              copyText(name)
              setCopied(true)
              setTimeout(() => setCopied(false), 1200)
            }}
          >
            <CopyIcon /> {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
        <input
          ref={input}
          className="gha-search"
          value={typed}
          placeholder={name}
          spellCheck={false}
          autoComplete="off"
          onChange={(e) => setTyped(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && confirm()}
        />
        <div className="gha-modal-actions">
          <button className="gha-ghost" onClick={onCancel}>Cancel</button>
          <button className="gha-danger-solid gha-icon-btn" disabled={!ok || busy} onClick={confirm}>
            <TrashIcon /> Delete project
          </button>
        </div>
      </div>
    </div>
  )
}
