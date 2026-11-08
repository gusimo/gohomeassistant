import { useEffect, useRef } from 'react'
import type { LogEntry } from './api'

type Props = {
  logs: LogEntry[]
  project: string | null // filter; null = all
  onlyProject: boolean
  setOnlyProject: (v: boolean) => void
  onClear: () => void
}

export function Logs({ logs, project, onlyProject, setOnlyProject, onClear }: Props) {
  const ref = useRef<HTMLDivElement>(null)
  const stick = useRef(true) // follow new lines while scrolled to the bottom

  const shown = onlyProject && project ? logs.filter((l) => l.project === project) : logs

  useEffect(() => {
    const el = ref.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [shown.length])

  return (
    <section className="gha-logs">
      <header className="gha-panel-header gha-sub-header">
        <label className="gha-check">
          <input type="checkbox" checked={onlyProject} disabled={!project} onChange={(e) => setOnlyProject(e.target.checked)} />
          only {project ?? 'selected project'}
        </label>
        <span className="gha-spacer" />
        <button className="gha-ghost" onClick={onClear}>Clear</button>
      </header>
      <div
        className="gha-log-lines"
        ref={ref}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 30
        }}
      >
        {shown.map((l) => (
          <LogLine key={l.seq} entry={l} />
        ))}
        {shown.length === 0 && <div className="gha-empty">No log output yet.</div>}
      </div>
    </section>
  )
}

function LogLine({ entry: l }: { entry: LogEntry }) {
  const { err, ...attrs } = l.attrs ?? {}
  const t = new Date(l.time).toLocaleTimeString([], { hour12: false })
  return (
    <div className={`gha-log-line gha-level-${l.level.toLowerCase()}${l.output ? ' gha-output' : ''}`}>
      <span className="gha-log-time">{t}</span>
      <span className="gha-log-project">{l.project ?? 'host'}</span>
      <span className="gha-log-msg">
        {l.msg}
        {Object.entries(attrs).map(([k, v]) => (
          <span key={k} className="gha-log-attr"> {k}={v}</span>
        ))}
        {err && <pre className="gha-log-err">{err}</pre>}
      </span>
    </div>
  )
}
