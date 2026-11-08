import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, parseBuildError, subscribe, type Commit, type FileInfo, type GitFile, type GitStatus, type LogEntry, type Me, type Status } from './api'
import { DiffView, Editor } from './Editor'
import { History, SourceControl, statusLetter } from './Git'
import { Entities } from './Entities'
import { DeleteProjectDialog } from './DeleteProjectDialog'
import { RebuildIcon, TrashIcon } from './Icons'
import { DebugPanel, useDebugger, type Breakpoints } from './Debug'
import { Logs } from './Logs'
import { connectLSP, disconnectLSP, fileURI, formatModel, notifySaved, registerProviders } from './lsp'
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api'

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
type Diff = { path: string; title: string; original: string; modified: string; restoreRev?: string; inWorkingTree: boolean }
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
  const [bottomTab, setBottomTab] = useState<'logs' | 'debug' | 'history'>('logs')
  const [gitStatus, setGitStatus] = useState<GitStatus | null>(null)
  const [diff, setDiff] = useState<Diff | null>(null)
  const [deleting, setDeleting] = useState(false)
  const onFilesRef = useRef<(p: string) => void>(() => {})
  const [bpVersion, setBpVersion] = useState(0)
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
      onFiles: (p) => onFilesRef.current(p),
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

  // Breakpoints per project, kept in the browser.
  const breakpoints = useMemo<Breakpoints>(() => {
    void bpVersion
    try {
      return project ? JSON.parse(stored(`gha.bp.${project}`) ?? '{}') : {}
    } catch {
      return {}
    }
  }, [project, bpVersion])
  const saveBreakpoints = (b: Breakpoints) => {
    if (!project) return
    store(`gha.bp.${project}`, JSON.stringify(b))
    setBpVersion((v) => v + 1)
  }
  const toggleBreakpoint = (line: number) => {
    if (!open) return
    const lines = breakpoints[open.path] ?? []
    const next = lines.includes(line) ? lines.filter((l) => l !== line) : [...lines, line].sort((a, b) => a - b)
    const b = { ...breakpoints, [open.path]: next }
    if (next.length === 0) delete b[open.path]
    saveBreakpoints(b)
  }
  const breakpointCount = Object.values(breakpoints).reduce((n, l) => n + l.length, 0)

  const dbg = useDebugger(projectExists ? project : null, projectExists ? status : undefined, breakpoints)
  // File of the selected stack frame, relative to the project.
  const frameFile =
    dbg.frame?.source?.path && projectDir && dbg.frame.source.path.startsWith(projectDir + '/')
      ? dbg.frame.source.path.slice(projectDir.length + 1)
      : null

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

  const refreshGit = useCallback(async (p: string) => {
    try {
      setGitStatus(await api.git.status(p))
    } catch {
      setGitStatus(null)
    }
  }, [])
  useEffect(() => {
    setGitStatus(null)
    setDiff(null)
    if (project && projectExists) refreshGit(project)
  }, [project, projectExists, refreshGit])

  // Files changed on disk (save, git, external editor): refresh lists and
  // reload open files that have no unsaved edits.
  const reloadTimer = useRef(0)
  const projectRef = useRef(project)
  projectRef.current = project
  onFilesRef.current = (p) => {
    if (p !== project) return
    clearTimeout(reloadTimer.current)
    reloadTimer.current = window.setTimeout(async () => {
      if (projectRef.current !== p) return // e.g. just deleted
      refreshFiles(p)
      refreshGit(p)
      for (const k of Object.keys(saved.current)) {
        if (!k.startsWith(p + '/')) continue
        const path = k.slice(p.length + 1)
        let text: string
        try {
          text = await api.readFile(p, path)
        } catch {
          delete saved.current[k] // deleted on disk
          setOpen((o) => (o && o.project === p && o.path === path ? null : o))
          continue
        }
        if (text === saved.current[k]) continue
        if (dirty.has(k)) {
          setError(`"${path}" changed on disk. Your unsaved edits were kept; saving overwrites the file.`)
          continue
        }
        saved.current[k] = text
        current.current[k] = text
        const model = projectDir ? monaco.editor.getModel(monaco.Uri.parse(fileURI(projectDir, path))) : null
        if (model) model.pushEditOperations([], [{ range: model.getFullModelRange(), text }], () => null)
      }
    }, 300)
  }

  const openWorkingDiff = async (path: string) => {
    if (!project) return
    try {
      const head = await api.git.show(project, 'HEAD', path)
      const st = gitStatus?.files?.find((f) => f.path === path)?.status
      const modified = st === 'deleted' ? '' : await api.readFile(project, path)
      setDiff({
        path,
        title: `${path}: last commit ↔ working copy`,
        original: head.text,
        modified,
        inWorkingTree: st !== 'deleted',
      })
    } catch (e) {
      setError(String(e))
    }
  }

  const openCommitDiff = async (path: string, c: Commit, st: GitFile['status']) => {
    if (!project) return
    try {
      const before = st === 'added' ? { text: '' } : await api.git.show(project, `${c.hash}^`, path)
      const after = st === 'deleted' ? { text: '' } : await api.git.show(project, c.hash, path)
      setDiff({
        path,
        title: `${path} in ${c.short}: ${c.message}`,
        original: before.text,
        modified: after.text,
        restoreRev: st === 'deleted' ? undefined : c.hash,
        inWorkingTree: files.some((f) => f.path === path),
      })
    } catch (e) {
      setError(String(e))
    }
  }

  const restore = async (path: string, rev: string) => {
    if (!project) return
    try {
      await api.git.restore(project, path, rev)
      setDiff(null)
      await openFile(project, path)
    } catch (e) {
      setError(String(e))
    }
  }

  // When the debugger stops, show the Debug tab and the paused line.
  useEffect(() => {
    if (dbg.state === 'stopped') setBottomTab('debug')
  }, [dbg.state])
  useEffect(() => {
    if (project && frameFile) {
      setDiff(null)
      openFile(project, frameFile)
    }
  }, [project, frameFile, openFile])

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
          <img className="gha-logo" src="./icon.png" alt="" />
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
                  className={!diff && open?.project === project && open.path === f.path ? 'gha-active' : ''}
                  onClick={() => {
                    setDiff(null)
                    openFile(project, f.path)
                  }}
                >
                  <span className="gha-file-name">
                    {f.path}
                    {dirty.has(key(project, f.path)) && <span className="gha-dirty">●</span>}
                  </span>
                  {gitBadge(gitStatus, f.path)}
                  {f.path !== 'go.mod' && <ConfirmButton label="×" title="Delete file" onConfirm={() => deleteFile(f.path)} />}
                </li>
              ))}
            </ul>
            <InlineInput placeholder="+ new file" pattern={/^[\w./-]+$/} onSubmit={createFile} />
            <SourceControl
              project={project}
              status={gitStatus}
              onChanged={() => refreshGit(project)}
              onOpenDiff={openWorkingDiff}
              onError={setError}
              confirm={(label, title, onConfirm) => <ConfirmButton label={label} title={title} onConfirm={onConfirm} />}
            />
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
              <button
                className="gha-ghost gha-icon-btn"
                title="Rebuild and restart the project"
                onClick={() => api.rebuild(project).catch((e) => setError(String(e)))}
              >
                <RebuildIcon /> Rebuild
              </button>
              <button className="gha-ghost gha-icon-btn gha-danger-text" title="Delete project and all its files" onClick={() => setDeleting(true)}>
                <TrashIcon /> Delete
              </button>
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
          {projectExists && diff ? (
            <div className="gha-diff">
              <div className="gha-diff-header">
                <span className="gha-crumb">{diff.title}</span>
                <span className="gha-spacer" />
                {diff.restoreRev && (
                  <ConfirmButton label="Restore this version" title="Replace the working copy with this version (uncommitted)" onConfirm={() => restore(diff.path, diff.restoreRev!)} />
                )}
                {diff.inWorkingTree && (
                  <button className="gha-small gha-ghost" onClick={() => { setDiff(null); if (project) openFile(project, diff.path) }}>
                    Open file
                  </button>
                )}
                <button className="gha-small gha-ghost" onClick={() => setDiff(null)}>Close</button>
              </div>
              <div className="gha-diff-body">
                <DiffView path={diff.path} original={diff.original} modified={diff.modified} dark={dark} />
              </div>
            </div>
          ) : projectExists && open && open.project === project ? (
            <Editor
              modelPath={projectDir ? fileURI(projectDir, open.path) : key(open.project, open.path)}
              filePath={open.path}
              initialValue={open.initial}
              problems={problems}
              dark={dark}
              onChange={onChange}
              onSave={save}
              onEditor={(e) => (editorRef.current = e)}
              breakpoints={breakpoints[open.path] ?? []}
              onToggleBreakpoint={toggleBreakpoint}
              currentLine={frameFile === open.path ? dbg.frame?.line : undefined}
            />
          ) : (
            <div className="gha-empty">{projectExists ? 'Select a file' : 'No project selected'}</div>
          )}
        </div>
        {showEntities && <Entities onInsert={insert} onClose={() => toggleEntities(false)} />}
        </div>

        <section className="gha-bottom">
          <nav className="gha-tabs">
            <button className={bottomTab === 'logs' ? 'gha-tab gha-active' : 'gha-tab'} onClick={() => setBottomTab('logs')}>
              Logs
            </button>
            <button className={bottomTab === 'debug' ? 'gha-tab gha-active' : 'gha-tab'} onClick={() => setBottomTab('debug')}>
              Debug
              {status?.debug && <span className={`gha-dot gha-dbg-dot gha-dbg-${dbg.state}`} />}
            </button>
            {gitStatus?.repo && (
              <button className={bottomTab === 'history' ? 'gha-tab gha-active' : 'gha-tab'} onClick={() => setBottomTab('history')}>
                History
              </button>
            )}
          </nav>
          {bottomTab === 'logs' || !projectExists || !project ? (
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
          ) : bottomTab === 'history' && gitStatus?.repo ? (
            <History project={project} head={gitStatus.head} onOpenCommitDiff={openCommitDiff} onError={setError} />
          ) : (
            <DebugPanel
              project={project}
              status={status}
              dbg={dbg}
              breakpointCount={breakpointCount}
              onClearBreakpoints={() => saveBreakpoints({})}
              onError={setError}
            />
          )}
        </section>
      </main>
      {deleting && project && (
        <DeleteProjectDialog
          name={project}
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            await deleteProject()
            setDeleting(false)
          }}
        />
      )}
    </div>
  )
}

function gitBadge(st: GitStatus | null, path: string) {
  const f = st?.files?.find((x) => x.path === path)
  return f ? <span className={`gha-git-badge gha-git-${f.status}`} title={f.status}>{statusLetter[f.status]}</span> : null
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
