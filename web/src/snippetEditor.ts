// Code snippets: built-in ones ship with the UI (snippets.json, checked by
// TestBuiltinSnippetsBuild), own ones are stored on the server. Both appear
// in the Snippets panel and in editor completion (type the prefix).
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api'
import builtin from './snippets.json'

// body uses Monaco snippet syntax: $1, ${1:default}, $0; a literal $ is \$.
// imports are Go import specs: "time" or "ha gohomeassistant.local/sdk".
export type Snippet = { name: string; prefix?: string; description?: string; body: string; imports?: string[] }

export const builtinSnippets: Snippet[] = builtin
let own: Snippet[] = []

export function setOwnSnippets(list: Snippet[]) {
  own = list
}

// Escapes text so it inserts literally as a snippet body.
export const escapeSnippet = (text: string) => text.replace(/[\\$}]/g, '\\$&')

// importEdits returns edits adding the imports the file doesn't have yet.
// gofmt (on save) sorts and groups them.
export function importEdits(model: monaco.editor.ITextModel, imports: string[] = []): monaco.editor.IIdentifiedSingleEditOperation[] {
  const text = model.getValue()
  const missing = imports
    .map((spec) => {
      const [a, b] = spec.trim().split(/\s+/)
      return b ? { name: a, path: b } : { name: '', path: a }
    })
    .filter(({ path }) => {
      const quoted = JSON.stringify(path).replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
      return !new RegExp(`^\\s*(import\\s+)?([\\w.]+\\s+)?${quoted}`, 'm').test(text)
    })
    .map(({ name, path }) => (name ? `${name} ${JSON.stringify(path)}` : JSON.stringify(path)))
  if (!missing.length) return []

  const lines = text.split('\n')
  const at = (line: number) => new monaco.Range(line, 1, line, 1) // insert before this 1-based line
  const block = lines.findIndex((l) => /^import\s*\($/.test(l.trim()))
  if (block >= 0) return [{ range: at(block + 2), text: missing.map((m) => `\t${m}\n`).join('') }]
  const single = lines.findIndex((l) => /^import\s/.test(l))
  if (single >= 0) return [{ range: at(single + 1), text: missing.map((m) => `import ${m}\n`).join('') }]
  const pkg = lines.findIndex((l) => /^package\s/.test(l))
  return [{ range: at(pkg + 2), text: `\nimport (\n${missing.map((m) => `\t${m}\n`).join('')})\n` }]
}

// insertSnippet inserts at the cursor with tab stops, adding missing imports.
export function insertSnippet(editor: monaco.editor.IStandaloneCodeEditor, sn: Snippet) {
  const model = editor.getModel()
  if (!model) return
  editor.focus()
  editor.pushUndoStop()
  // Imports first: they are above the cursor, which moves along with them.
  const edits = importEdits(model, sn.imports)
  if (edits.length) editor.executeEdits('snippets', edits)
  ;(editor.getContribution('snippetController2') as any).insert(sn.body)
  editor.pushUndoStop()
}

let registered = false

export function registerSnippetCompletion() {
  if (registered) return
  registered = true
  monaco.languages.registerCompletionItemProvider('go', {
    provideCompletionItems(model, position) {
      const word = model.getWordUntilPosition(position)
      // Not after "x." (that's a member) and only once something is typed.
      const before = model.getLineContent(position.lineNumber)[word.startColumn - 2]
      if (!word.word || before === '.') return { suggestions: [] }
      const range = new monaco.Range(position.lineNumber, word.startColumn, position.lineNumber, word.endColumn)
      return {
        suggestions: [...own, ...builtinSnippets]
          .filter((s) => s.prefix)
          .map((s) => ({
            label: { label: s.prefix!, description: s.name },
            kind: monaco.languages.CompletionItemKind.Snippet,
            detail: s.name,
            documentation: { value: (s.description ? s.description + '\n\n' : '') + '```go\n' + s.body + '\n```' },
            insertText: s.body,
            insertTextRules: monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet,
            range,
            additionalTextEdits: importEdits(model, s.imports),
          })),
      }
    },
  })
}
