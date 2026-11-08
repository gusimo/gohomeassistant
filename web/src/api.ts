// All URLs are relative so the UI works under HA Ingress' path prefix.
import type { Snippet } from './snippetEditor'

export type Status = {
  name: string
  dir: string // absolute path on the server, for file:// URIs
  building: boolean
  running: boolean
  pid?: number
  buildError?: string
  builtAt?: string
  restarts: number
  debug: boolean
  debugPort?: number
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

export type GitFile = { path: string; status: 'modified' | 'added' | 'deleted' | 'renamed' | 'untracked' }
export type GitStatus = { repo: boolean; branch?: string; head?: string; files?: GitFile[] }
export type Commit = { hash: string; short: string; author: string; date: string; message: string }

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

const proj = (p: string) => `./api/projects/${encodeURIComponent(p)}`

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
  setDebug: (name: string, enabled: boolean) =>
    req('POST', `./api/projects/${encodeURIComponent(name)}/debug`, JSON.stringify({ enabled })),
  entities: async (): Promise<EntityState[]> => (await req('GET', './api/entities')).json(),
  git: {
    status: async (p: string): Promise<GitStatus> => (await req('GET', `${proj(p)}/git/status`)).json(),
    init: (p: string) => req('POST', `${proj(p)}/git/init`),
    commit: (p: string, message: string) => req('POST', `${proj(p)}/git/commit`, JSON.stringify({ message })),
    log: async (p: string): Promise<Commit[]> => (await req('GET', `${proj(p)}/git/log`)).json(),
    commitFiles: async (p: string, rev: string): Promise<GitFile[]> =>
      (await req('GET', `${proj(p)}/git/commits/${rev}`)).json(),
    // show returns the file at a revision; exists is false if it didn't exist there.
    show: async (p: string, rev: string, path: string) => {
      const res = await req('GET', `${proj(p)}/git/show?${new URLSearchParams({ rev, path })}`)
      return { text: await res.text(), exists: res.headers.get('X-Exists') !== '0' }
    },
    discard: (p: string, path: string) => req('POST', `${proj(p)}/git/discard`, JSON.stringify({ path })),
    restore: (p: string, path: string, rev: string) => req('POST', `${proj(p)}/git/restore`, JSON.stringify({ path, rev })),
  },
  snippets: async (): Promise<Snippet[]> => (await req('GET', './api/snippets')).json(),
  saveSnippets: (list: Snippet[]) => req('PUT', './api/snippets', JSON.stringify(list)),
  me: async (): Promise<Me> => (await req('GET', './api/me')).json(),
  logout: () => req('POST', './auth/logout'),
}

// subscribe streams logs and status changes. After a reconnect the server
// replays its history, so onReset tells the caller to drop what it has.
export function subscribe(handlers: {
  onReset: () => void
  onLog: (e: LogEntry) => void
  onStatus: (s: Status) => void
  onFiles: (project: string) => void
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
    else if (ev.type === 'files') handlers.onFiles(ev.project)
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
