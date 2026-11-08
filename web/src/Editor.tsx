import { useEffect, useRef, useState } from 'react'
import MonacoEditor, { loader, type OnMount } from '@monaco-editor/react'
// Bundle Monaco locally (no CDN): the UI must work offline and behind Ingress.
import * as monaco from 'monaco-editor/esm/vs/editor/editor.api'
// editor.api is the bare core; this adds the contributions (suggest widget,
// hover, parameter hints, find, folding, ...).
import 'monaco-editor/esm/vs/editor/editor.all.js'
import 'monaco-editor/esm/vs/basic-languages/go/go.contribution'
import 'monaco-editor/esm/vs/basic-languages/markdown/markdown.contribution'
import 'monaco-editor/esm/vs/basic-languages/yaml/yaml.contribution'
import EditorWorker from 'monaco-editor/esm/vs/editor/editor.worker?worker'
import type { BuildProblem } from './api'

self.MonacoEnvironment = { getWorker: () => new EditorWorker() }
loader.config({ monaco })

const language = (path: string) =>
  path.endsWith('.go') ? 'go' : path.endsWith('.md') ? 'markdown' : /\.ya?ml$/.test(path) ? 'yaml' : 'plaintext'

type Props = {
  modelPath: string // unique per project+file; Monaco keeps one model (and undo history) per path
  filePath: string
  initialValue: string
  problems: BuildProblem[]
  dark: boolean
  onChange: (value: string) => void
  onSave: () => void
  onEditor: (editor: monaco.editor.IStandaloneCodeEditor) => void
  breakpoints: number[] // lines of this file
  onToggleBreakpoint: (line: number) => void
  currentLine?: number // where the debugger is paused, in this file
}

export function Editor(props: Props) {
  const { modelPath, filePath, initialValue, problems, dark, onChange, onSave, onEditor, breakpoints, currentLine } = props
  const editorRef = useRef<monaco.editor.IStandaloneCodeEditor | null>(null)
  const [editor, setEditor] = useState<monaco.editor.IStandaloneCodeEditor | null>(null)
  const decorations = useRef<monaco.editor.IEditorDecorationsCollection | null>(null)
  const onSaveRef = useRef(onSave)
  onSaveRef.current = onSave
  const onToggleRef = useRef(props.onToggleBreakpoint)
  onToggleRef.current = props.onToggleBreakpoint

  const onMount: OnMount = (editor, m) => {
    editorRef.current = editor
    onEditor(editor)
    setEditor(editor)
    decorations.current = editor.createDecorationsCollection()
    editor.addCommand(m.KeyMod.CtrlCmd | m.KeyCode.KeyS, () => onSaveRef.current())
    // Click in the glyph margin (left of the line numbers) toggles a breakpoint.
    editor.onMouseDown((e) => {
      if (e.target.type === m.editor.MouseTargetType.GUTTER_GLYPH_MARGIN && e.target.position) {
        onToggleRef.current(e.target.position.lineNumber)
      }
    })
  }

  // Breakpoints and the paused line.
  useEffect(() => {
    if (!editor || !decorations.current) return
    const d: monaco.editor.IModelDeltaDecoration[] = breakpoints.map((line) => ({
      range: new monaco.Range(line, 1, line, 1),
      options: { glyphMarginClassName: 'gha-bp', glyphMarginHoverMessage: { value: 'Breakpoint (click to remove)' } },
    }))
    if (currentLine) {
      d.push({
        range: new monaco.Range(currentLine, 1, currentLine, 1),
        options: { isWholeLine: true, className: 'gha-current-line', linesDecorationsClassName: 'gha-current-arrow' },
      })
      editor.revealLineInCenterIfOutsideViewport(currentLine)
    }
    decorations.current.set(d)
  }, [editor, breakpoints, currentLine, modelPath])

  // Show build errors of this file as markers.
  useEffect(() => {
    const model = editorRef.current?.getModel()
    if (!model) return
    monaco.editor.setModelMarkers(
      model,
      'build',
      problems
        .filter((p) => p.file === filePath)
        .map((p) => ({
          severity: monaco.MarkerSeverity.Error,
          message: p.msg,
          startLineNumber: p.line,
          startColumn: p.col,
          endLineNumber: p.line,
          endColumn: model.getLineMaxColumn(Math.min(p.line, model.getLineCount())),
        })),
    )
  }, [problems, filePath, modelPath])

  return (
    <MonacoEditor
      path={modelPath}
      defaultValue={initialValue}
      defaultLanguage={language(filePath)}
      theme={dark ? 'vs-dark' : 'vs'}
      onMount={onMount}
      onChange={(v) => onChange(v ?? '')}
      options={{
        fontSize: 13,
        minimap: { enabled: false },
        scrollBeyondLastLine: false,
        automaticLayout: true,
        tabSize: 4,
        insertSpaces: false, // gofmt style
        glyphMargin: true, // breakpoints
      }}
    />
  )
}
