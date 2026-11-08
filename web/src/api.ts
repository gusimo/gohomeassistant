// All URLs are relative so the UI works under HA Ingress' path prefix.

export type Status = {
  name: string
  dir: string // absolute path on the server, for file:// URIs
  building: boolean
  running: boolean
  pid?: number
  buildError?: string
  builtAt?: string
  restarts: number
  removed?: boolean
}

export type LogEntry = {
  seq: number
  time: string
  level: string
  project?: string
  output?: boolean
  msg: string
  attrs?: Record<string, string>
}

export type FileInfo = { path: string; size: number }

export type EntityState = {
  entity_id: string
  state: string
  attributes: Record<string, unknown> & { friendly_name?: string; unit_of_measurement?: string }
  last_changed: string
  ref: string // Go expression, e.g. "ent.Light.Kitchen"; "" before the first generation
}

export type Me = { user: { id: string; name: string; admin: boolean }; mode: 'none' | 'ingress' | 'ha' }

async function req(method: string, url: string, body?: BodyInit): Promise<Response> {
  const res = await fetch(url, { method, body })
  if (res.status === 401) {
    location.href = './auth/login' // session expired
    throw new Error('not logged in')
  }
  if (!res.ok) throw new Error((await res.text()).trim() || `${res.status} ${res.statusText}`)
  return res
}

const fileURL = (project: string, path: string) =>
  `./api/projects/${encodeURIComponent(project)}/files/${path.split('/').map(encodeURIComponent).join('/')}`

export const api = {
  listFiles: async (project: string): Promise<FileInfo[]> =>
    (await req('GET', `./api/projects/${encodeURIComponent(project)}/files`)).json(),
  readFile: async (project: string, path: string) => (await req('GET', fileURL(project, path))).text(),
  writeFile: (project: string, path: string, content: string) => req('PUT', fileURL(project, path), content),
  deleteFile: (project: string, path: string) => req('DELETE', fileURL(project, path)),
  createProject: (name: string) => req('POST', './api/projects', JSON.stringify({ name })),
  deleteProject: (name: string) => req('DELETE', `./api/projects/${encodeURIComponent(name)}`),
  rebuild: (name: string) => req('POST', `./api/projects/${encodeURIComponent(name)}/build`),
  entities: async (): Promise<EntityState[]> => (await req('GET', './api/entities')).json(),
  me: async (): Promise<Me> => (await req('GET', './api/me')).json(),
  logout: () => req('POST', './auth/logout'),
}

// subscribe streams logs and status changes. After a reconnect the server
// replays its history, so onReset tells the caller to drop what it has.
export function subscribe(handlers: {
  onReset: () => void
  onLog: (e: LogEntry) => void
  onStatus: (s: Status) => void
  onConnection: (connected: boolean) => void
}): () => void {
  const es = new EventSource('./api/events')
  es.onopen = () => {
    handlers.onReset()
    handlers.onConnection(true)
  }
  es.onerror = () => handlers.onConnection(false)
  es.onmessage = (m) => {
    const ev = JSON.parse(m.data)
    if (ev.type === 'log') handlers.onLog(ev.log)
    else if (ev.type === 'status') handlers.onStatus(ev.status)
  }
  return () => es.close()
}

// Build errors look like "./main.go:14:13: undefined: ent.Sun".
export type BuildProblem = { file: string; line: number; col: number; msg: string }

export function parseBuildError(err: string | undefined): BuildProblem[] {
  if (!err) return []
  const out: BuildProblem[] = []
  for (const line of err.split('\n')) {
    const m = /^(?:\.\/)?([^\s:]+\.go):(\d+):(\d+): (.*)$/.exec(line.trim())
    if (m) out.push({ file: m[1], line: +m[2], col: +m[3], msg: m[4] })
  }
  return out
}
