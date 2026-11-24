import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { api, type LogEntry, type LogQuery } from './api'

type Props = {
  logs: LogEntry[] // live: what the server keeps in memory, plus what arrived since
  project: string | null // filter; null = all
  onlyProject: boolean
  setOnlyProject: (v: boolean) => void
  onClear: () => void
  lastVisit: number // seq of the last entry seen on the previous visit
}

const LEVELS: Record<string, string[]> = { WARN: ['WARN', 'ERROR'], ERROR: ['ERROR'] }

// useOlder pages further back than the live entries, into the history on
// disk. A changed filter starts over.
export function useOlder(live: LogEntry[], filter: LogQuery) {
  const key = JSON.stringify(filter)
  const [state, setState] = useState<{ key: string; entries: LogEntry[]; done: boolean }>({ key, entries: [], done: false })
  const [loading, setLoading] = useState(false)
  const cur = state.key === key ? state : { key, entries: [], done: false }
  const load = async () => {
    setLoading(true)
    try {
      const before = cur.entries[0]?.seq ?? live[0]?.seq
      const older = await api.logs({ ...filter, before })
      setState({ key, entries: older.concat(cur.entries), done: older.length < 500 })
    } finally {
      setLoading(false)
    }
  }
  return { older: cur.entries, done: cur.done, loading, load }
}

export const dayOf = (time: string) => new Date(time).toLocaleDateString([], { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric' })
export const timeOf = (time: string) => new Date(time).toLocaleTimeString([], { hour12: false })

export function Logs({ logs, project, onlyProject, setOnlyProject, onClear, lastVisit }: Props) {
  const ref = useRef<HTMLDivElement>(null)
  const stick = useRef(true) // follow new lines while scrolled to the bottom
  const [level, setLevel] = useState('')
  const [query, setQuery] = useState('')

  const byProject = onlyProject && project ? project : undefined
  const text = query.trim().toLowerCase()
  const filter = useMemo<LogQuery>(() => ({ project: byProject, level: level || undefined, q: text || undefined }), [byProject, level, text])
  const { older, done, loading, load } = useOlder(logs, filter)

  const shown = useMemo(() => {
    const match = (l: LogEntry) =>
      (!byProject || l.project === byProject) &&
      (!level || LEVELS[level].includes(l.level)) &&
      (!text || (l.msg ?? '').toLowerCase().includes(text) || Object.values(l.attrs ?? {}).some((v) => v.toLowerCase().includes(text)))
    return older.concat(logs.filter(match))
  }, [older, logs, byProject, level, text])

  const lastSeq = shown.length ? shown[shown.length - 1].seq : 0
  useEffect(() => {
    const el = ref.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [lastSeq, filter])

  // Older lines are added above: keep what the user looks at in place.
  const height = useRef(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (el && height.current) el.scrollTop += el.scrollHeight - height.current
    height.current = 0
  }, [older.length])

  let day = ''
  return (
    <section className="gha-logs">
      <header className="gha-panel-header gha-sub-header gha-log-filters">
        <label className="gha-check">
          <input type="checkbox" checked={onlyProject} disabled={!project} onChange={(e) => setOnlyProject(e.target.checked)} />
          only {project ?? 'selected project'}
        </label>
        <select className="gha-select" value={level} onChange={(e) => setLevel(e.target.value)} title="Minimum level">
          <option value="">all levels</option>
          <option value="WARN">warnings and errors</option>
          <option value="ERROR">errors</option>
        </select>
        <input className="gha-search gha-log-search" placeholder="Search logs" value={query} onChange={(e) => setQuery(e.target.value)} />
        <span className="gha-spacer" />
        <button className="gha-ghost" onClick={onClear} title="Empty this view (the history on disk is kept)">Clear</button>
      </header>
      <div
        className="gha-log-lines"
        ref={ref}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 30
        }}
      >
        <div className="gha-log-older">
          {done ? (
            <span className="gha-muted">Start of the log history</span>
          ) : (
            <button
              className="gha-ghost gha-small"
              disabled={loading}
              onClick={() => {
                height.current = ref.current?.scrollHeight ?? 0
                load()
              }}
            >
              {loading ? 'Loading…' : 'Load older'}
            </button>
          )}
        </div>
        {shown.map((l, i) => {
          const d = dayOf(l.time)
          const newDay = d !== day
          day = d
          return (
            <div key={l.seq}>
              {newDay && <div className="gha-log-day">{d}</div>}
              {lastVisit > 0 && l.seq > lastVisit && i > 0 && shown[i - 1].seq <= lastVisit && (
                <div className="gha-log-day gha-log-visit">new since your last visit</div>
              )}
              <LogLine entry={l} />
            </div>
          )
        })}
        {shown.length === 0 && <div className="gha-empty">{level || text ? 'No matching log lines.' : 'No log output yet.'}</div>}
      </div>
    </section>
  )
}

function LogLine({ entry: l }: { entry: LogEntry }) {
  const { err, ...attrs } = l.attrs ?? {}
  return (
    <div className={`gha-log-line gha-level-${l.level.toLowerCase()}${l.output ? ' gha-output' : ''}`}>
      <span className="gha-log-time">{timeOf(l.time)}</span>
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
