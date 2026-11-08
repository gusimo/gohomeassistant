import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, parseBuildError, subscribe, type FileInfo, type LogEntry, type Me, type Status } from './api'
import { Editor } from './Editor'
import { Entities } from './Entities'
import { Logs } from './Logs'
import { connectLSP, disconnectLSP, fileURI, formatModel, notifySaved, registerProviders } from './lsp'
import type * as monaco from 'monaco-editor/esm/vs/editor/editor.api'

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
  const [showEntities, setShowEntities] = useState(stored('gha.entities') === '1')
  const [me, setMe] = useState<Me | null>(null)
  const saved = useRef<Record<string, string>>({}) // last loaded/saved content per file
  const current = useRef<Record<string, string>>({}) // current editor content per file
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null)

  useEffect(() => {
    api.me().then(setMe, () => {})
  }, [])

  const toggleEntities = (v: boolean) => {
    setShowEntities(v)
    store('gha.entities', v ? '1' : '0')
  }

  // Insert text at the cursor of the open file.
  const insert = (text: string) => {
    const ed = editorRef.current
    const sel = ed?.getSelection()
    if (!ed || !sel) return
    ed.executeEdits('entities', [{ range: sel, text, forceMoveMarkers: true }])
    ed.focus()
  }

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
  const projectDir = project ? statuses[project]?.dir : undefined

  // One gopls session per selected project.
  useEffect(() => {
    if (!project || !projectDir) return
    registerProviders()
    const client = connectLSP(project, projectDir)
    return () => disconnectLSP(client)
  }, [project, projectDir])
  useEffect(() => {
    if (!project || !projectExists) return
    store('gha.project', project)
    refreshFiles(project)
    openFile(project, stored(`gha.file.${project}`) ?? 'main.go')
  }, [project, projectExists, refreshFiles, openFile])

  const save = useCallback(async () => {
    if (!open) return
    const k = key(open.project, open.path)
    const editor = editorRef.current
    if (editor) await formatModel(editor) // gofmt via gopls
    const content = current.current[k] ?? saved.current[k]
    try {
      await api.writeFile(open.project, open.path, content)
      notifySaved(editor?.getModel() ?? null)
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
    <div className="gha-app">
      <aside className="gha-sidebar">
        <div className="gha-brand">
          GoHomeAssistant
          <span className={`gha-conn ${connected ? 'gha-ok' : 'gha-down'}`} title={connected ? 'connected' : 'disconnected'} />
        </div>
        {me && (
          <div className="gha-user">
            <span title={`auth: ${me.mode}`}>{me.user.name}</span>
            {me.mode === 'ha' && (
              <button className="gha-ghost gha-small" onClick={() => api.logout().then(() => location.reload())}>
                Log out
              </button>
            )}
          </div>
        )}

        <div className="gha-section-title">Projects</div>
        <ul className="gha-list">
          {projects.map((p) => (
            <li key={p.name} className={p.name === project ? 'gha-active' : ''} onClick={() => setProject(p.name)}>
              <span className={`gha-dot ${dotClass(p)}`} title={statusText(p)} />
              {p.name}
            </li>
          ))}
        </ul>
        <InlineInput placeholder="+ new project" pattern={/^[a-z0-9][a-z0-9_-]*$/} onSubmit={createProject} />

        {projectExists && project && (
          <>
            <div className="gha-section-title">Files</div>
            <ul className="gha-list gha-files">
              {files.map((f) => (
                <li
                  key={f.path}
                  className={open?.project === project && open.path === f.path ? 'gha-active' : ''}
                  onClick={() => openFile(project, f.path)}
                >
                  <span className="gha-file-name">
                    {f.path}
                    {dirty.has(key(project, f.path)) && <span className="gha-dirty">●</span>}
                  </span>
                  {f.path !== 'go.mod' && <ConfirmButton label="×" title="Delete file" onConfirm={() => deleteFile(f.path)} />}
                </li>
              ))}
            </ul>
            <InlineInput placeholder="+ new file" pattern={/^[\w./-]+$/} onSubmit={createFile} />
          </>
        )}
      </aside>

      <main className="gha-main">
        <header className="gha-panel-header gha-editor-header">
          {projectExists && project ? (
            <>
              <span className="gha-crumb">
                {project}
                {open && open.project === project && <> / {open.path}</>}
              </span>
              {status && <span className={`gha-status ${dotClass(status)}`}>{statusText(status)}</span>}
              <span className="gha-spacer" />
              <button className={`gha-ghost${showEntities ? ' gha-toggled' : ''}`} onClick={() => toggleEntities(!showEntities)}>
                Entities
              </button>
              <button className="gha-ghost" onClick={() => api.rebuild(project).catch((e) => setError(String(e)))}>
                Rebuild
              </button>
              <ConfirmButton label="Delete project" title="Delete project and all its files" onConfirm={deleteProject} />
              <button disabled={!open || !dirty.has(key(open.project, open.path))} onClick={save} title="⌘S / Ctrl+S">
                Save
              </button>
            </>
          ) : (
            <span className="gha-crumb">Select or create a project</span>
          )}
        </header>

        {error && (
          <div className="gha-error-banner" onClick={() => setError(null)}>
            {error} <span className="gha-dismiss">dismiss</span>
          </div>
        )}

        <div className="gha-editor-row">
        <div className="gha-editor">
          {projectExists && open && open.project === project ? (
            <Editor
              modelPath={projectDir ? fileURI(projectDir, open.path) : key(open.project, open.path)}
              filePath={open.path}
              initialValue={open.initial}
              problems={problems}
              dark={dark}
              onChange={onChange}
              onSave={save}
              onEditor={(e) => (editorRef.current = e)}
            />
          ) : (
            <div className="gha-empty">{projectExists ? 'Select a file' : 'No project selected'}</div>
          )}
        </div>
        {showEntities && <Entities onInsert={insert} onClose={() => toggleEntities(false)} />}
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
  if (s.building) return 'gha-building'
  if (s.buildError) return 'gha-failed'
  if (s.running) return 'gha-running'
  return 'gha-stopped'
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
      className={`gha-inline-input${value && !valid ? ' gha-invalid' : ''}`}
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
      className={`gha-ghost gha-danger${armed ? ' gha-armed' : ''}`}
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
