import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, parseBuildError, subscribe, type FileInfo, type LogEntry, type Status } from './api'
import { Editor } from './Editor'
import { Logs } from './Logs'

const MAX_LOGS = 5000

function useDarkMode() {
  const q = useMemo(() => window.matchMedia('(prefers-color-scheme: dark)'), [])
  const [dark, setDark] = useState(q.matches)
  useEffect(() => {
    const f = (e: MediaQueryListEvent) => setDark(e.matches)
    q.addEventListener('change', f)
    return () => q.removeEventListener('change', f)
  }, [q])
  return dark
}

function stored(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}
function store(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    /* private mode etc. */
  }
}

type OpenFile = { project: string; path: string; initial: string }
const key = (project: string, path: string) => `${project}/${path}`

export function App() {
  const dark = useDarkMode()
  const [statuses, setStatuses] = useState<Record<string, Status>>({})
  const [logs, setLogs] = useState<LogEntry[]>([])
  const [connected, setConnected] = useState(false)
  const [project, setProject] = useState<string | null>(stored('gha.project'))
  const [files, setFiles] = useState<FileInfo[]>([])
  const [open, setOpen] = useState<OpenFile | null>(null)
  const [dirty, setDirty] = useState<Set<string>>(new Set())
  const [error, setError] = useState<string | null>(null)
  const [onlyProject, setOnlyProject] = useState(stored('gha.onlyProject') === '1')
  const saved = useRef<Record<string, string>>({}) // last loaded/saved content per file
  const current = useRef<Record<string, string>>({}) // current editor content per file

  // Live logs and status. Log lines are batched per animation frame.
  useEffect(() => {
    let buf: LogEntry[] = []
    let frame = 0
    const flush = () => {
      frame = 0
      const add = buf
      buf = []
      setLogs((l) => {
        const next = l.concat(add)
        return next.length > MAX_LOGS ? next.slice(next.length - MAX_LOGS) : next
      })
    }
    return subscribe({
      onReset: () => {
        buf = []
        setLogs([])
        setStatuses({})
      },
      onLog: (e) => {
        buf.push(e)
        if (!frame) frame = requestAnimationFrame(flush)
      },
      onStatus: (s) =>
        setStatuses((all) => {
          const next = { ...all }
          if (s.removed) delete next[s.name]
          else next[s.name] = s
          return next
        }),
      onConnection: setConnected,
    })
  }, [])

  const projects = Object.values(statuses).sort((a, b) => a.name.localeCompare(b.name))
  const status = project ? statuses[project] : undefined
  const problems = useMemo(() => parseBuildError(status?.buildError), [status?.buildError])

  const refreshFiles = useCallback(async (p: string) => {
    try {
      setFiles(await api.listFiles(p))
    } catch (e) {
      setError(String(e))
    }
  }, [])

  const openFile = useCallback(async (p: string, path: string) => {
    const k = key(p, path)
    try {
      if (!(k in saved.current)) saved.current[k] = await api.readFile(p, path)
      setOpen({ project: p, path, initial: saved.current[k] })
      store(`gha.file.${p}`, path)
    } catch (e) {
      setError(String(e))
    }
  }, [])

  // Selecting a project loads its files and reopens the last file. Wait until
  // the server reported the project: the remembered one may be gone.
  const projectExists = !!project && !!statuses[project]
  useEffect(() => {
    if (!project || !projectExists) return
    store('gha.project', project)
    refreshFiles(project)
    openFile(project, stored(`gha.file.${project}`) ?? 'main.go')
  }, [project, projectExists, refreshFiles, openFile])

  const save = useCallback(async () => {
    if (!open) return
    const k = key(open.project, open.path)
    const content = current.current[k] ?? saved.current[k]
    try {
      await api.writeFile(open.project, open.path, content)
      saved.current[k] = content
      setDirty((d) => {
        const n = new Set(d)
        n.delete(k)
        return n
      })
    } catch (e) {
      setError(`Save failed: ${e}`)
    }
  }, [open])

  const onChange = (value: string) => {
    if (!open) return
    const k = key(open.project, open.path)
    current.current[k] = value
    const isDirty = value !== saved.current[k]
    setDirty((d) => {
      if (d.has(k) === isDirty) return d
      const n = new Set(d)
      if (isDirty) n.add(k)
      else n.delete(k)
      return n
    })
  }

  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => {
      if (dirty.size > 0) e.preventDefault()
    }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  const createProject = async (name: string) => {
    try {
      await api.createProject(name)
      setProject(name)
    } catch (e) {
      setError(String(e))
    }
  }

  const createFile = async (path: string) => {
    if (!project) return
    try {
      await api.writeFile(project, path, path.endsWith('.go') ? 'package main\n' : '')
      await refreshFiles(project)
      await openFile(project, path)
    } catch (e) {
      setError(String(e))
    }
  }

  const deleteFile = async (path: string) => {
    if (!project) return
    try {
      await api.deleteFile(project, path)
      delete saved.current[key(project, path)]
      if (open?.path === path) setOpen(null)
      await refreshFiles(project)
    } catch (e) {
      setError(String(e))
    }
  }

  const deleteProject = async () => {
    if (!project) return
    try {
      await api.deleteProject(project)
      store('gha.project', '')
      setProject(null)
      setOpen(null)
      setFiles([])
    } catch (e) {
      setError(String(e))
    }
  }

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          GoHomeAssistant
          <span className={`conn ${connected ? 'ok' : 'down'}`} title={connected ? 'connected' : 'disconnected'} />
        </div>

        <div className="section-title">Projects</div>
        <ul className="list">
          {projects.map((p) => (
            <li key={p.name} className={p.name === project ? 'active' : ''} onClick={() => setProject(p.name)}>
              <span className={`dot ${dotClass(p)}`} title={statusText(p)} />
              {p.name}
            </li>
          ))}
        </ul>
        <InlineInput placeholder="+ new project" pattern={/^[a-z0-9][a-z0-9_-]*$/} onSubmit={createProject} />

        {projectExists && project && (
          <>
            <div className="section-title">Files</div>
            <ul className="list files">
              {files.map((f) => (
                <li
                  key={f.path}
                  className={open?.project === project && open.path === f.path ? 'active' : ''}
                  onClick={() => openFile(project, f.path)}
                >
                  <span className="file-name">
                    {f.path}
                    {dirty.has(key(project, f.path)) && <span className="dirty">●</span>}
                  </span>
                  {f.path !== 'go.mod' && <ConfirmButton label="×" title="Delete file" onConfirm={() => deleteFile(f.path)} />}
                </li>
              ))}
            </ul>
            <InlineInput placeholder="+ new file" pattern={/^[\w./-]+$/} onSubmit={createFile} />
          </>
        )}
      </aside>

      <main className="main">
        <header className="panel-header editor-header">
          {projectExists && project ? (
            <>
              <span className="crumb">
                {project}
                {open && open.project === project && <> / {open.path}</>}
              </span>
              {status && <span className={`status ${dotClass(status)}`}>{statusText(status)}</span>}
              <span className="spacer" />
              <button className="ghost" onClick={() => api.rebuild(project).catch((e) => setError(String(e)))}>
                Rebuild
              </button>
              <ConfirmButton label="Delete project" title="Delete project and all its files" onConfirm={deleteProject} />
              <button disabled={!open || !dirty.has(key(open.project, open.path))} onClick={save} title="⌘S / Ctrl+S">
                Save
              </button>
            </>
          ) : (
            <span className="crumb">Select or create a project</span>
          )}
        </header>

        {error && (
          <div className="error-banner" onClick={() => setError(null)}>
            {error} <span className="dismiss">dismiss</span>
          </div>
        )}

        <div className="editor">
          {projectExists && open && open.project === project ? (
            <Editor
              modelPath={key(open.project, open.path)}
              filePath={open.path}
              initialValue={open.initial}
              problems={problems}
              dark={dark}
              onChange={onChange}
              onSave={save}
            />
          ) : (
            <div className="empty">{projectExists ? 'Select a file' : 'No project selected'}</div>
          )}
        </div>

        <Logs
          logs={logs}
          project={projectExists ? project : null}
          onlyProject={onlyProject}
          setOnlyProject={(v) => {
            setOnlyProject(v)
            store('gha.onlyProject', v ? '1' : '0')
          }}
          onClear={() => setLogs([])}
        />
      </main>
    </div>
  )
}

function dotClass(s: Status) {
  if (s.building) return 'building'
  if (s.buildError) return 'failed'
  if (s.running) return 'running'
  return 'stopped'
}

function statusText(s: Status) {
  if (s.building) return 'Building…'
  if (s.buildError && s.running) return 'Build failed, previous version still running'
  if (s.buildError) return 'Build failed'
  if (s.running) return `Running (pid ${s.pid})`
  return s.restarts > 0 ? 'Crashed, restarting…' : 'Stopped'
}

function InlineInput({ placeholder, pattern, onSubmit }: { placeholder: string; pattern: RegExp; onSubmit: (v: string) => void }) {
  const [value, setValue] = useState('')
  const valid = pattern.test(value)
  return (
    <input
      className={`inline-input${value && !valid ? ' invalid' : ''}`}
      placeholder={placeholder}
      value={value}
      onChange={(e) => setValue(e.target.value.trim())}
      onKeyDown={(e) => {
        if (e.key === 'Enter' && valid) {
          onSubmit(value)
          setValue('')
        } else if (e.key === 'Escape') setValue('')
      }}
    />
  )
}

// ConfirmButton needs a second click to act (no browser dialogs).
function ConfirmButton({ label, title, onConfirm }: { label: string; title: string; onConfirm: () => void }) {
  const [armed, setArmed] = useState(false)
  useEffect(() => {
    if (!armed) return
    const t = setTimeout(() => setArmed(false), 3000)
    return () => clearTimeout(t)
  }, [armed])
  return (
    <button
      className={`ghost danger${armed ? ' armed' : ''}`}
      title={title}
      onClick={(e) => {
        e.stopPropagation()
        if (armed) onConfirm()
        setArmed(!armed)
      }}
    >
      {armed ? 'Confirm?' : label}
    </button>
  )
}
