// A minimal LSP client connecting Monaco to gopls over a WebSocket (one
// JSON-RPC message per frame). It covers what the editor needs: document
// sync, completion (incl. auto-imports), hover, signature help, diagnostics
// and formatting.
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api'

type Pending = { resolve: (v: any) => void; reject: (e: Error) => void }
type Position = { line: number; character: number }
type LspRange = { start: Position; end: Position }

const REQUEST_TIMEOUT = 10_000

export class LSPClient {
  private ws: WebSocket
  private nextId = 1
  private pending = new Map<number, Pending>()
  private versions = new Map<string, number>()
  private ready: Promise<void>
  private closed = false
  private initialized = false // models created before this are opened by initialize

  // rootUri: file:// URI of the project directory.
  constructor(url: string, readonly rootUri: string) {
    this.ws = new WebSocket(url)
    this.ready = new Promise((resolve, reject) => {
      this.ws.onopen = () => this.initialize().then(resolve, reject)
      this.ws.onerror = () => reject(new Error('LSP connection failed'))
    })
    this.ws.onmessage = (e) => this.onMessage(JSON.parse(e.data))
    this.ws.onclose = () => {
      this.closed = true
      for (const p of this.pending.values()) p.reject(new Error('LSP connection closed'))
      this.pending.clear()
      for (const m of monaco.editor.getModels()) if (this.owns(m)) monaco.editor.setModelMarkers(m, 'gopls', [])
    }
  }

  owns(model: monaco.editor.ITextModel) {
    return model.uri.toString().startsWith(this.rootUri + '/')
  }

  close() {
    this.ws.close()
  }

  async request<T = any>(method: string, params: unknown): Promise<T> {
    await this.ready
    if (this.closed) throw new Error('LSP connection closed')
    const id = this.nextId++
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id)
        reject(new Error(`${method} timed out`))
      }, REQUEST_TIMEOUT)
      this.pending.set(id, {
        resolve: (v) => (clearTimeout(timer), resolve(v)),
        reject: (e) => (clearTimeout(timer), reject(e)),
      })
      this.send({ id, method, params })
    })
  }

  notify(method: string, params: unknown) {
    this.ready.then(() => !this.closed && this.send({ method, params }))
  }

  private send(msg: object) {
    this.ws.send(JSON.stringify({ jsonrpc: '2.0', ...msg }))
  }

  private async initialize() {
    const name = this.rootUri.split('/').pop()
    // Not this.request(): that waits for this.ready, which waits for us.
    const id = this.nextId++
    await new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.send({
        id,
        method: 'initialize',
        params: {
          processId: null,
          rootUri: this.rootUri,
          workspaceFolders: [{ uri: this.rootUri, name }],
          capabilities: {
            textDocument: {
              synchronization: { didSave: true, dynamicRegistration: false },
              completion: {
                completionItem: { snippetSupport: true, documentationFormat: ['markdown', 'plaintext'] },
              },
              hover: { contentFormat: ['markdown', 'plaintext'] },
              signatureHelp: {
                signatureInformation: {
                  documentationFormat: ['markdown', 'plaintext'],
                  parameterInformation: { labelOffsetSupport: true },
                },
              },
              publishDiagnostics: {},
              formatting: {},
            },
            workspace: { configuration: true, workspaceFolders: true },
          },
        },
      })
    })
    this.send({ method: 'initialized', params: {} })
    this.initialized = true
    // Open the models of this project that already exist.
    for (const m of monaco.editor.getModels()) if (this.owns(m)) this.didOpen(m)
  }

  private onMessage(msg: any) {
    if (msg.method && msg.id !== undefined) {
      // Server -> client request: answer minimally so gopls doesn't wait.
      // No pkg.go.dev links in hovers: the generated ent module isn't public.
      const result =
        msg.method === 'workspace/configuration'
          ? (msg.params.items ?? []).map((it: any) => (it.section === 'gopls' ? { linksInHover: false } : {}))
          : null
      this.send({ id: msg.id, result })
      return
    }
    if (msg.method === 'textDocument/publishDiagnostics') {
      const model = monaco.editor.getModel(monaco.Uri.parse(msg.params.uri))
      if (model) monaco.editor.setModelMarkers(model, 'gopls', msg.params.diagnostics.map(toMarker))
      return
    }
    const p = msg.id !== undefined && this.pending.get(msg.id)
    if (p) {
      this.pending.delete(msg.id)
      if (msg.error) p.reject(new Error(msg.error.message))
      else p.resolve(msg.result)
    }
  }

  // Full-document sync: simple and cheap for automation-sized files.
  didOpen(model: monaco.editor.ITextModel) {
    if (!this.initialized || this.closed) return
    const uri = model.uri.toString()
    this.versions.set(uri, model.getVersionId())
    this.send({
      method: 'textDocument/didOpen',
      params: { textDocument: { uri, languageId: model.getLanguageId(), version: model.getVersionId(), text: model.getValue() } },
    })
  }

  didChange(model: monaco.editor.ITextModel) {
    const uri = model.uri.toString()
    if (!this.versions.has(uri)) return
    this.notify('textDocument/didChange', {
      textDocument: { uri, version: model.getVersionId() },
      contentChanges: [{ text: model.getValue() }],
    })
  }

  didSave(model: monaco.editor.ITextModel) {
    this.notify('textDocument/didSave', { textDocument: { uri: model.uri.toString() } })
  }
}

// --- Monaco integration: providers are registered once and use the active client.

let active: LSPClient | null = null

export function connectLSP(projectName: string, projectDir: string): LSPClient {
  const url = new URL(`./api/projects/${encodeURIComponent(projectName)}/lsp`, location.href)
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
  const client = new LSPClient(url.toString(), monaco.Uri.file(projectDir).toString())
  active = client
  return client
}

export function disconnectLSP(client: LSPClient) {
  if (active === client) active = null
  client.close()
}

function clientFor(model: monaco.editor.ITextModel): LSPClient | null {
  return active && active.owns(model) ? active : null
}

// The editor's model path for a project file; must match gopls' file URIs.
export const fileURI = (projectDir: string, path: string) => monaco.Uri.file(`${projectDir}/${path}`).toString()

const toPos = (p: monaco.IPosition): Position => ({ line: p.lineNumber - 1, character: p.column - 1 })
const toRange = (r: LspRange) => new monaco.Range(r.start.line + 1, r.start.character + 1, r.end.line + 1, r.end.character + 1)

const severity: Record<number, monaco.MarkerSeverity> = {
  1: monaco.MarkerSeverity.Error,
  2: monaco.MarkerSeverity.Warning,
  3: monaco.MarkerSeverity.Info,
  4: monaco.MarkerSeverity.Hint,
}

function toMarker(d: any): monaco.editor.IMarkerData {
  const r = toRange(d.range)
  return {
    severity: severity[d.severity] ?? monaco.MarkerSeverity.Error,
    message: d.message,
    source: d.source,
    startLineNumber: r.startLineNumber,
    startColumn: r.startColumn,
    endLineNumber: r.endLineNumber,
    endColumn: r.endColumn,
  }
}

function markdown(doc: any): monaco.IMarkdownString | undefined {
  if (!doc) return undefined
  if (typeof doc === 'string') return { value: doc }
  if (doc.kind === 'markdown') return { value: doc.value }
  if (doc.kind === 'plaintext') return { value: doc.value.replace(/[\\`*_{}[\]()#+\-.!<>]/g, '\\$&') }
  if (doc.language) return { value: '```' + doc.language + '\n' + doc.value + '\n```' } // MarkedString
  return undefined
}

// LSP CompletionItemKind (1-based) -> Monaco kind name.
const kinds = [
  'Text', 'Method', 'Function', 'Constructor', 'Field', 'Variable', 'Class', 'Interface', 'Module', 'Property',
  'Unit', 'Value', 'Enum', 'Keyword', 'Snippet', 'Color', 'File', 'Reference', 'Folder', 'EnumMember',
  'Constant', 'Struct', 'Event', 'Operator', 'TypeParameter',
] as const

let registered = false

export function registerProviders() {
  if (registered) return
  registered = true

  monaco.editor.onDidCreateModel((model) => {
    clientFor(model)?.didOpen(model)
    model.onDidChangeContent(() => clientFor(model)?.didChange(model))
  })

  monaco.languages.registerCompletionItemProvider('go', {
    triggerCharacters: ['.'],
    async provideCompletionItems(model, position) {
      const c = clientFor(model)
      if (!c) return { suggestions: [] }
      const res = await c.request('textDocument/completion', {
        textDocument: { uri: model.uri.toString() },
        position: toPos(position),
      })
      const items: any[] = Array.isArray(res) ? res : (res?.items ?? [])
      const word = model.getWordUntilPosition(position)
      const fallback = new monaco.Range(position.lineNumber, word.startColumn, position.lineNumber, word.endColumn)
      return {
        incomplete: !!res?.isIncomplete,
        suggestions: items.map((it) => {
          const edit = it.textEdit
          return {
            label: it.label,
            kind: monaco.languages.CompletionItemKind[kinds[(it.kind ?? 1) - 1] ?? 'Text'],
            detail: it.detail,
            documentation: markdown(it.documentation),
            sortText: it.sortText,
            filterText: it.filterText,
            insertText: edit?.newText ?? it.insertText ?? it.label,
            insertTextRules: it.insertTextFormat === 2 ? monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet : undefined,
            range: edit ? toRange(edit.range ?? edit.insert) : fallback,
            additionalTextEdits: it.additionalTextEdits?.map((e: any) => ({ range: toRange(e.range), text: e.newText })),
          }
        }),
      }
    },
  })

  monaco.languages.registerHoverProvider('go', {
    async provideHover(model, position) {
      const c = clientFor(model)
      if (!c) return null
      const res = await c.request('textDocument/hover', { textDocument: { uri: model.uri.toString() }, position: toPos(position) })
      if (!res) return null
      const contents = (Array.isArray(res.contents) ? res.contents : [res.contents]).map(markdown).filter(Boolean)
      return { contents, range: res.range ? toRange(res.range) : undefined }
    },
  })

  monaco.languages.registerSignatureHelpProvider('go', {
    signatureHelpTriggerCharacters: ['(', ','],
    async provideSignatureHelp(model, position) {
      const c = clientFor(model)
      if (!c) return null
      const res = await c.request('textDocument/signatureHelp', { textDocument: { uri: model.uri.toString() }, position: toPos(position) })
      if (!res?.signatures?.length) return null
      return {
        value: {
          activeSignature: res.activeSignature ?? 0,
          activeParameter: res.activeParameter ?? 0,
          signatures: res.signatures.map((s: any) => ({
            label: s.label,
            documentation: markdown(s.documentation),
            parameters: (s.parameters ?? []).map((p: any) => ({ label: p.label, documentation: markdown(p.documentation) })),
          })),
        },
        dispose() {},
      }
    },
  })

  monaco.languages.registerDocumentFormattingEditProvider('go', {
    async provideDocumentFormattingEdits(model, options) {
      const c = clientFor(model)
      if (!c) return []
      const edits = await c.request('textDocument/formatting', {
        textDocument: { uri: model.uri.toString() },
        options: { tabSize: options.tabSize, insertSpaces: options.insertSpaces },
      })
      return (edits ?? []).map((e: any) => ({ range: toRange(e.range), text: e.newText }))
    },
  })
}

// formatModel formats the editor's Go model with gopls (gofmt). Errors, e.g.
// on syntax errors, leave the code unformatted.
export async function formatModel(editor: monaco.editor.IStandaloneCodeEditor) {
  const model = editor.getModel()
  if (!model || !clientFor(model) || model.getLanguageId() !== 'go') return
  try {
    await editor.getAction('editor.action.formatDocument')?.run()
  } catch {
    /* keep unformatted */
  }
}

export function notifySaved(model: monaco.editor.ITextModel | null) {
  if (model) clientFor(model)?.didSave(model)
}
