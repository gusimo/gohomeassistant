import { useCallback, useEffect, useRef, useState } from 'react'
import { api, type Status } from './api'
import { DAPClient, type Frame, type Variable } from './dap'

export type DebugState = 'off' | 'attaching' | 'running' | 'stopped' | 'error'

// Breakpoints per file, relative to the project directory.
export type Breakpoints = Record<string, number[]>

export type Debugger = {
  state: DebugState
  error: string | null
  reason: string
  frames: Frame[]
  frame: Frame | null
  variables: Variable[]
  selectFrame: (f: Frame) => void
  expand: (ref: number) => Promise<Variable[]>
  cont: () => void
  next: () => void
  stepIn: () => void
  stepOut: () => void
  pause: () => void
}

// useDebugger attaches to the project whenever it runs under Delve (again
// after every hot reload) and keeps its breakpoints in sync.
export function useDebugger(project: string | null, status: Status | undefined, breakpoints: Breakpoints): Debugger {
  const [state, setState] = useState<DebugState>('off')
  const [error, setError] = useState<string | null>(null)
  const [reason, setReason] = useState('')
  const [frames, setFrames] = useState<Frame[]>([])
  const [frame, setFrame] = useState<Frame | null>(null)
  const [variables, setVariables] = useState<Variable[]>([])
  const client = useRef<DAPClient | null>(null)
  const thread = useRef(0)
  const bpRef = useRef(breakpoints)
  const sent = useRef<Breakpoints>({})

  const dir = status?.dir
  const abs = useCallback((rel: Breakpoints) => {
    const out: Record<string, number[]> = {}
    for (const [file, lines] of Object.entries(rel)) out[`${dir}/${file}`] = lines
    return out
  }, [dir])

  const showFrame = useCallback(async (c: DAPClient, f: Frame) => {
    setFrame(f)
    try {
      const scopes = await c.scopes(f.id)
      setVariables(scopes.length ? await c.variables(scopes[0].variablesReference) : [])
    } catch {
      setVariables([])
    }
  }, [])

  const port = status?.running ? status.debugPort : undefined
  const pid = status?.pid
  useEffect(() => {
    setFrames([])
    setFrame(null)
    setVariables([])
    if (!project || !port) {
      setState('off')
      return
    }
    setState('attaching')
    setError(null)
    const url = new URL(`./api/projects/${encodeURIComponent(project)}/dap`, location.href)
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
    let closed = false
    const c = new DAPClient(url.toString(), {
      onStopped: async (threadId, why) => {
        thread.current = threadId
        setReason(why)
        setState('stopped')
        try {
          const fs = await c.stackTrace(threadId)
          setFrames(fs)
          // Show the first frame in user code, not inside the runtime.
          const top = fs.find((f) => f.source?.path?.startsWith(`${dir}/`)) ?? fs[0]
          if (top) await showFrame(c, top)
        } catch (e) {
          setError(String(e))
        }
      },
      onContinued: () => {
        setState('running')
        setFrames([])
        setFrame(null)
        setVariables([])
      },
      onClose: () => {
        if (!closed) setState('off')
      },
    })
    client.current = c
    sent.current = bpRef.current
    c.start(abs(bpRef.current))
      .then(() => setState((s) => (s === 'attaching' ? 'running' : s)))
      .catch((e) => {
        setState('error')
        setError(String(e))
      })
    return () => {
      closed = true
      client.current = null
      c.detach()
    }
  }, [project, port, pid, dir, abs, showFrame])

  // Send changed breakpoints.
  useEffect(() => {
    bpRef.current = breakpoints
    const c = client.current
    if (!c || state === 'attaching' || state === 'off') return
    const files = new Set([...Object.keys(sent.current), ...Object.keys(breakpoints)])
    for (const f of files) {
      const a = (sent.current[f] ?? []).join(',')
      const b = (breakpoints[f] ?? []).join(',')
      if (a !== b) c.setBreakpoints(`${dir}/${f}`, breakpoints[f] ?? []).catch((e) => setError(String(e)))
    }
    sent.current = breakpoints
  }, [breakpoints, state, dir])

  const act = (fn: (c: DAPClient, t: number) => Promise<unknown>) => () => {
    const c = client.current
    if (!c) return
    fn(c, thread.current || 1).catch((e) => setError(String(e)))
  }
  // Delve doesn't always send "continued" after a step; reflect it ourselves.
  const resume = (fn: (c: DAPClient, t: number) => Promise<unknown>) =>
    act(async (c, t) => {
      setState('running')
      setFrames([])
      setFrame(null)
      setVariables([])
      await fn(c, t)
    })

  return {
    state,
    error,
    reason,
    frames,
    frame,
    variables,
    selectFrame: (f) => client.current && showFrame(client.current, f),
    expand: (ref) => client.current?.variables(ref) ?? Promise.resolve([]),
    cont: resume((c, t) => c.continue(t)),
    next: resume((c, t) => c.next(t)),
    stepIn: resume((c, t) => c.stepIn(t)),
    stepOut: resume((c, t) => c.stepOut(t)),
    pause: act(async (c) => {
      const ts = await c.threads()
      await c.pause(ts[0]?.id ?? 1)
    }),
  }
}

type PanelProps = {
  project: string
  status: Status | undefined
  dbg: Debugger
  breakpointCount: number
  onClearBreakpoints: () => void
  onError: (e: string) => void
}

export function DebugPanel({ project, status, dbg, breakpointCount, onClearBreakpoints, onError }: PanelProps) {
  const stopped = dbg.state === 'stopped'
  const running = dbg.state === 'running'

  // F5 continue, F10 step over, F11 step into, Shift+F11 step out.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!stopped) return
      const action = { F5: dbg.cont, F10: dbg.next, F11: e.shiftKey ? dbg.stepOut : dbg.stepIn }[e.key]
      if (action) {
        e.preventDefault()
        action()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [stopped, dbg])

  const toggle = (on: boolean) => api.setDebug(project, on).catch((e) => onError(String(e)))

  return (
    <div className="gha-debug">
      <div className="gha-debug-toolbar">
        <label className="gha-check">
          <input type="checkbox" checked={!!status?.debug} onChange={(e) => toggle(e.target.checked)} />
          Debug mode
        </label>
        <span className={`gha-debug-state gha-dbg-${dbg.state}`}>{stateText(dbg, status)}</span>
        <span className="gha-spacer" />
        <button className="gha-small" disabled={!stopped} onClick={dbg.cont} title="Continue (F5)">Continue</button>
        <button className="gha-small gha-ghost" disabled={!running} onClick={dbg.pause} title="Pause">Pause</button>
        <button className="gha-small gha-ghost" disabled={!stopped} onClick={dbg.next} title="Step over (F10)">Step over</button>
        <button className="gha-small gha-ghost" disabled={!stopped} onClick={dbg.stepIn} title="Step into (F11)">Step into</button>
        <button className="gha-small gha-ghost" disabled={!stopped} onClick={dbg.stepOut} title="Step out (Shift+F11)">Step out</button>
        <button className="gha-small gha-ghost" disabled={breakpointCount === 0} onClick={onClearBreakpoints}>
          Clear {breakpointCount} breakpoint{breakpointCount === 1 ? '' : 's'}
        </button>
      </div>
      {dbg.error && <div className="gha-error-banner">{dbg.error}</div>}
      {!status?.debug ? (
        <div className="gha-empty">
          Turn on debug mode to set breakpoints that pause this project. The project is rebuilt without optimizations and
          keeps running until a breakpoint is hit. Click left of a line number to set a breakpoint.
        </div>
      ) : (
        <div className="gha-debug-body">
          <div className="gha-debug-col">
            <div className="gha-section-title">Call stack</div>
            <ul className="gha-frames">
              {dbg.frames.map((f) => (
                <li key={f.id} className={dbg.frame?.id === f.id ? 'gha-active' : ''} onClick={() => dbg.selectFrame(f)}>
                  <span className="gha-frame-name">{f.name}</span>
                  <span className="gha-frame-loc">
                    {shortPath(f.source?.path, status?.dir)}:{f.line}
                  </span>
                </li>
              ))}
              {dbg.frames.length === 0 && <li className="gha-muted">{stopped ? 'No frames' : 'Not paused'}</li>}
            </ul>
          </div>
          <div className="gha-debug-col">
            <div className="gha-section-title">Variables</div>
            <ul className="gha-vars">
              {dbg.variables.map((v) => (
                <VarNode key={v.name} v={v} expand={dbg.expand} />
              ))}
              {stopped && dbg.variables.length === 0 && <li className="gha-muted">No local variables</li>}
            </ul>
          </div>
        </div>
      )}
    </div>
  )
}

function VarNode({ v, expand }: { v: Variable; expand: (ref: number) => Promise<Variable[]> }) {
  const [children, setChildren] = useState<Variable[] | null>(null)
  const open = children !== null
  const expandable = v.variablesReference > 0
  return (
    <li>
      <div
        className={`gha-var${expandable ? ' gha-expandable' : ''}`}
        onClick={() => expandable && (open ? setChildren(null) : expand(v.variablesReference).then(setChildren))}
      >
        <span className="gha-var-arrow">{expandable ? (open ? '▾' : '▸') : ''}</span>
        <span className="gha-var-name">{v.name}</span>
        <span className="gha-var-value" title={v.type}>{v.value}</span>
      </div>
      {open && (
        <ul className="gha-vars gha-nested">
          {children.map((c, i) => (
            <VarNode key={`${c.name}-${i}`} v={c} expand={expand} />
          ))}
        </ul>
      )}
    </li>
  )
}

function stateText(d: Debugger, s: Status | undefined) {
  if (!s?.debug) return 'off'
  if (s.building || !s.debugPort) return 'starting debugger…'
  switch (d.state) {
    case 'attaching':
      return 'attaching…'
    case 'running':
      return 'running, waiting for a breakpoint'
    case 'stopped':
      return `paused (${d.reason})`
    case 'error':
      return 'error'
    default:
      return 'detached'
  }
}

function shortPath(path: string | undefined, dir: string | undefined) {
  if (!path) return '?'
  if (dir && path.startsWith(dir + '/')) return path.slice(dir.length + 1)
  const i = path.indexOf('/gohomeassistant/')
  return i >= 0 ? path.slice(i + 1) : path.split('/').slice(-2).join('/')
}
