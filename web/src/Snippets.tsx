import { useMemo, useState } from 'react'
import { builtinSnippets, escapeSnippet, type Snippet } from './snippetEditor'

type Props = {
  own: Snippet[]
  onSaveOwn: (list: Snippet[]) => Promise<void>
  onInsert: (s: Snippet) => void
  selection: () => string // selected text in the editor
  onClose: () => void
}

// What the form edits; index null = new snippet.
type Draft = { index: number | null; name: string; prefix: string; description: string; imports: string; body: string }

const toDraft = (s: Snippet, index: number | null): Draft => ({
  index,
  name: s.name,
  prefix: s.prefix ?? '',
  description: s.description ?? '',
  imports: (s.imports ?? []).join(', '),
  body: s.body,
})

export function Snippets({ own, onSaveOwn, onInsert, selection, onClose }: Props) {
  const [query, setQuery] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)
  const [draft, setDraft] = useState<Draft | null>(null)
  const [deleting, setDeleting] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)

  const items = useMemo(() => {
    const all = [
      ...own.map((s, i) => ({ s, key: `o${i}`, ownIndex: i as number | null })),
      ...builtinSnippets.map((s, i) => ({ s, key: `b${i}`, ownIndex: null as number | null })),
    ]
    const terms = query.toLowerCase().split(/\s+/).filter(Boolean)
    return all.filter(({ s }) => {
      const hay = `${s.name} ${s.prefix ?? ''} ${s.description ?? ''}`.toLowerCase()
      return terms.every((t) => hay.includes(t))
    })
  }, [own, query])

  const save = async () => {
    if (!draft) return
    const s: Snippet = {
      name: draft.name.trim(),
      prefix: draft.prefix.trim() || undefined,
      description: draft.description.trim() || undefined,
      imports: draft.imports.split(',').map((i) => i.trim()).filter(Boolean),
      body: draft.body,
    }
    if (!s.name || !s.body.trim()) return setError('Name and code are required')
    const list = [...own]
    if (draft.index === null) list.unshift(s)
    else list[draft.index] = s
    try {
      await onSaveOwn(list)
      setDraft(null)
      setError(null)
      setQuery('')
    } catch (e) {
      setError(String(e))
    }
  }

  const remove = async (index: number) => {
    try {
      await onSaveOwn(own.filter((_, i) => i !== index))
      setDeleting(null)
      setExpanded(null)
    } catch (e) {
      setError(String(e))
    }
  }

  const field = (k: Exclude<keyof Draft, 'index'>) => ({
    value: draft![k],
    onChange: (e: { target: { value: string } }) => setDraft({ ...draft!, [k]: e.target.value }),
  })

  return (
    <aside className="gha-entities">
      <header className="gha-panel-header">
        <span>Snippets</span>
        <span className="gha-muted">{items.length}</span>
        <span className="gha-spacer" />
        {!draft && (
          <button
            className="gha-small gha-ghost"
            title="New snippet, prefilled with the code selected in the editor"
            onClick={() => setDraft({ index: null, name: '', prefix: '', description: '', imports: '', body: escapeSnippet(dedent(selection())) })}
          >
            + New
          </button>
        )}
        <button className="gha-ghost" onClick={onClose} title="Close">×</button>
      </header>
      {error && <div className="gha-error-banner" onClick={() => setError(null)}>{error}</div>}

      {draft ? (
        <div className="gha-snippet-form">
          <label>Name<input {...field('name')} autoFocus placeholder="Motion light" /></label>
          <label>Prefix<input {...field('prefix')} placeholder="motionlight (type it in the editor)" /></label>
          <label>Description<input {...field('description')} /></label>
          <label>Imports<input {...field('imports')} placeholder="time, ha gohomeassistant.local/sdk" /></label>
          <label>
            Code
            <textarea {...field('body')} spellCheck={false} rows={12} />
          </label>
          <p className="gha-muted">
            Tab stops: <code>$1</code>, <code>{'${1:default}'}</code>, end <code>$0</code>; write <code>\$</code> for a
            literal $.
          </p>
          <div className="gha-entity-actions">
            <span className="gha-spacer" />
            <button className="gha-small gha-ghost" onClick={() => (setDraft(null), setError(null))}>Cancel</button>
            <button className="gha-small" onClick={save}>Save snippet</button>
          </div>
        </div>
      ) : (
        <>
          <input className="gha-search" placeholder="Search snippets" value={query} autoFocus onChange={(e) => setQuery(e.target.value)} />
          <ul className="gha-entity-list">
            {items.map(({ s, key, ownIndex }) => {
              const open = expanded === key
              return (
                <li key={key} className={open ? 'gha-open' : ''}>
                  <div className="gha-entity-row" onClick={() => setExpanded(open ? null : key)}>
                    <div className="gha-entity-names">
                      <span className="gha-entity-name">{s.name}</span>
                      {s.description && <span className="gha-entity-id gha-snippet-desc">{s.description}</span>}
                    </div>
                    {ownIndex !== null && <span className="gha-badge" title="Your snippet">own</span>}
                    {s.prefix && <code className="gha-snippet-prefix">{s.prefix}</code>}
                  </div>
                  {open && (
                    <div className="gha-entity-detail">
                      <div className="gha-entity-actions">
                        <button className="gha-small" onClick={() => onInsert(s)}>Insert</button>
                        <span className="gha-spacer" />
                        {ownIndex === null ? (
                          <button className="gha-small gha-ghost" title="Copy into your own snippets to change it" onClick={() => setDraft(toDraft(s, null))}>
                            Customize
                          </button>
                        ) : deleting === ownIndex ? (
                          <>
                            <button className="gha-small gha-ghost" onClick={() => setDeleting(null)}>Keep</button>
                            <button className="gha-small gha-danger-solid" onClick={() => remove(ownIndex)}>Delete it</button>
                          </>
                        ) : (
                          <>
                            <button className="gha-small gha-ghost" onClick={() => setDraft(toDraft(s, ownIndex))}>Edit</button>
                            <button className="gha-small gha-ghost gha-danger-text" onClick={() => setDeleting(ownIndex)}>Delete</button>
                          </>
                        )}
                      </div>
                      <pre className="gha-snippet-body">{s.body}</pre>
                      {!!s.imports?.length && <div className="gha-muted gha-snippet-imports">imports: {s.imports.join(', ')}</div>}
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        </>
      )}
    </aside>
  )
}

// dedent removes the indentation all lines share and trailing blank lines,
// so a snippet made from indented code inserts at the cursor's indentation.
function dedent(text: string) {
  const lines = text.replace(/\s+$/, '').split('\n')
  const indents = lines.filter((l) => l.trim()).map((l) => /^[\t ]*/.exec(l)![0].length)
  const n = indents.length ? Math.min(...indents) : 0
  return lines.map((l) => l.slice(n)).join('\n')
}
