// A minimal Debug Adapter Protocol client for Delve, over the WebSocket
// bridge (one DAP message per frame). It attaches to the project's running
// headless Delve ("remote" mode) and never terminates the program.

export type Frame = { id: number; name: string; line: number; column: number; source?: { path?: string; name?: string } }
export type Variable = { name: string; value: string; type?: string; variablesReference: number }
export type Scope = { name: string; variablesReference: number }

type Handlers = {
  onStopped: (threadId: number, reason: string) => void
  onContinued: () => void
  onClose: () => void
}

const TIMEOUT = 15_000

export class DAPClient {
  private ws: WebSocket
  private seq = 0
  private pending = new Map<number, { resolve: (b: any) => void; reject: (e: Error) => void }>()
  private eventWaiters = new Map<string, () => void>()
  private open: Promise<void>

  constructor(url: string, private h: Handlers) {
    this.ws = new WebSocket(url)
    this.open = new Promise((resolve, reject) => {
      this.ws.onopen = () => resolve()
      this.ws.onerror = () => reject(new Error('debugger connection failed'))
    })
    this.ws.onclose = () => {
      for (const p of this.pending.values()) p.reject(new Error('debugger disconnected'))
      this.pending.clear()
      this.h.onClose()
    }
    this.ws.onmessage = (e) => this.onMessage(JSON.parse(e.data))
  }

  // start attaches and installs breakpoints (absolute path -> lines).
  async start(breakpoints: Record<string, number[]>) {
    await this.open
    await this.request('initialize', {
      clientID: 'gohomeassistant',
      adapterID: 'go',
      linesStartAt1: true,
      columnsStartAt1: true,
      pathFormat: 'path',
      supportsVariableType: true,
    })
    const initialized = this.waitEvent('initialized')
    const attached = this.request('attach', { mode: 'remote' })
    await initialized
    for (const [path, lines] of Object.entries(breakpoints)) await this.setBreakpoints(path, lines)
    await this.request('configurationDone', {})
    await attached
  }

  setBreakpoints(path: string, lines: number[]) {
    return this.request('setBreakpoints', { source: { path }, breakpoints: lines.map((line) => ({ line })) })
  }

  async stackTrace(threadId: number): Promise<Frame[]> {
    return (await this.request('stackTrace', { threadId, levels: 50 })).stackFrames ?? []
  }
  async scopes(frameId: number): Promise<Scope[]> {
    return (await this.request('scopes', { frameId })).scopes ?? []
  }
  async variables(ref: number): Promise<Variable[]> {
    return (await this.request('variables', { variablesReference: ref })).variables ?? []
  }
  async threads(): Promise<{ id: number; name: string }[]> {
    return (await this.request('threads', {})).threads ?? []
  }

  continue = (threadId: number) => this.request('continue', { threadId })
  next = (threadId: number) => this.request('next', { threadId })
  stepIn = (threadId: number) => this.request('stepIn', { threadId })
  stepOut = (threadId: number) => this.request('stepOut', { threadId })
  pause = (threadId: number) => this.request('pause', { threadId })

  // detach leaves the program running.
  async detach() {
    try {
      await this.request('disconnect', { terminateDebuggee: false })
    } catch {
      /* already gone */
    }
    this.ws.close()
  }

  private request(command: string, args: unknown): Promise<any> {
    const seq = ++this.seq
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(seq)
        reject(new Error(`${command} timed out`))
      }, TIMEOUT)
      this.pending.set(seq, {
        resolve: (b) => (clearTimeout(timer), resolve(b)),
        reject: (e) => (clearTimeout(timer), reject(e)),
      })
      // Wait for the connection: sending while it is still opening throws.
      this.open.then(
        () => this.ws.send(JSON.stringify({ seq, type: 'request', command, arguments: args })),
        (e) => this.pending.get(seq)?.reject(e),
      )
    })
  }

  private waitEvent(name: string) {
    return new Promise<void>((resolve) => this.eventWaiters.set(name, resolve))
  }

  private onMessage(m: any) {
    if (m.type === 'response') {
      const p = this.pending.get(m.request_seq)
      if (!p) return
      this.pending.delete(m.request_seq)
      if (m.success) p.resolve(m.body ?? {})
      else p.reject(new Error(m.message || m.body?.error?.format || `${m.command} failed`))
      return
    }
    if (m.type !== 'event') return
    this.eventWaiters.get(m.event)?.()
    this.eventWaiters.delete(m.event)
    if (m.event === 'stopped') this.h.onStopped(m.body.threadId, m.body.reason)
    else if (m.event === 'continued') this.h.onContinued()
  }
}
