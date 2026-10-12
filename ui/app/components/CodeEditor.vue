<script setup lang="ts">
/*
 * The file manager's text editor: CodeMirror 6 with the language picked from
 * the file name. This component and CodeMirror load only when the editor
 * opens (files.vue uses <LazyCodeEditor>), and each language pack is a chunk
 * of its own (~/lib/editorLanguages).
 *
 * CSP: CodeMirror styles itself through style-mod, which uses constructable
 * stylesheets where the browser has them and a <style> element otherwise.
 * The panel's style-src already allows 'unsafe-inline' (Nuxt UI needs it),
 * so neither needs a nonce; script-src is untouched.
 */
import { Compartment, EditorState, type Extension, type Text } from '@codemirror/state'
import {
  EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter,
  highlightSpecialChars, drawSelection, dropCursor, rectangularSelection, crosshairCursor
} from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { search, searchKeymap, highlightSelectionMatches } from '@codemirror/search'
import {
  bracketMatching, foldGutter, foldKeymap, indentOnInput, syntaxHighlighting, HighlightStyle
} from '@codemirror/language'
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap } from '@codemirror/autocomplete'
import { tags as t } from '@lezer/highlight'
import { loadLanguage } from '~/lib/editorLanguages'

const props = defineProps<{
  /** The file's content as loaded; the editor starts clean on it. */
  content: string
  filename: string
  /** "utf-8" or "utf-8-bom", from the read endpoint. */
  encoding?: string
  /** Saves the content; resolves true when it was written. */
  save: (content: string) => Promise<boolean>
}>()
const dirty = defineModel<boolean>('dirty', { default: false })

const { t: tr } = useI18n()
const colorMode = useColorMode()

const host = ref<HTMLElement | null>(null)
const line = ref(1)
const col = ref(1)
const bytes = ref(0)
const wrap = ref(false)
const saving = ref(false)

const kind = computed(() => detectFile(props.filename))
const shebang = detectShebang(props.content.slice(0, 200).split('\n', 1)[0] ?? '')
const lang = kind.value.lang === 'text' && shebang ? shebang[0] : (kind.value.lang ?? 'text')
const langLabel = kind.value.lang === 'text' && shebang ? shebang[1] : kind.value.label

let view: EditorView | null = null
let saved: Text | null = null
const languageConf = new Compartment()
const wrapConf = new Compartment()
const darkConf = new Compartment()

// The search panel, folding and announcements in the panel's language.
const phraseKeys: Record<string, string> = {
  'Find': 'find', 'Replace': 'replace', 'next': 'next', 'previous': 'previous', 'all': 'all',
  'match case': 'matchCase', 'regexp': 'regexp', 'by word': 'byWord', 'replace': 'replaceOne',
  'replace all': 'replaceAll', 'close': 'close', 'Go to line': 'goToLine', 'go': 'go',
  'Folded lines': 'foldedLines', 'Unfolded lines': 'unfoldedLines', 'to': 'to',
  'folded code': 'foldedCode', 'unfold': 'unfold', 'Fold line': 'foldLine', 'Unfold line': 'unfoldLine',
  'current match': 'currentMatch', 'on line': 'onLine', 'replaced match on line $': 'replacedMatch',
  'replaced $ matches': 'replacedMatches', 'Control character': 'controlChar'
}
const phrases = Object.fromEntries(Object.entries(phraseKeys).map(([en, key]) => [en, tr(`editor.phrases.${key}`)]))

// Colours come from the panel's tokens (see the <style> block), so the
// editor follows light and dark mode without being rebuilt.
const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.controlKeyword, t.moduleKeyword, t.operatorKeyword, t.definitionKeyword, t.modifier, t.self], color: 'var(--cg-syn-keyword)' },
  { tag: [t.string, t.special(t.string), t.regexp, t.character, t.attributeValue, t.inserted], color: 'var(--cg-syn-string)' },
  { tag: [t.number, t.bool, t.null, t.atom, t.unit, t.constant(t.variableName), t.escape], color: 'var(--cg-syn-number)' },
  { tag: [t.comment, t.lineComment, t.blockComment, t.docComment], color: 'var(--cg-syn-comment)', fontStyle: 'italic' },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.macroName, t.labelName], color: 'var(--cg-syn-function)' },
  { tag: [t.typeName, t.className, t.namespace, t.standard(t.variableName)], color: 'var(--cg-syn-type)' },
  { tag: [t.propertyName, t.definition(t.propertyName)], color: 'var(--cg-syn-property)' },
  { tag: [t.tagName], color: 'var(--cg-syn-tag)' },
  { tag: [t.attributeName], color: 'var(--cg-syn-attribute)' },
  { tag: [t.special(t.variableName), t.local(t.variableName)], color: 'var(--cg-syn-variable)' },
  { tag: [t.operator, t.punctuation, t.separator, t.bracket, t.angleBracket], color: 'var(--cg-syn-punctuation)' },
  { tag: [t.meta, t.processingInstruction, t.documentMeta, t.annotation], color: 'var(--cg-syn-meta)' },
  { tag: t.heading, color: 'var(--cg-syn-function)', fontWeight: '600' },
  { tag: t.strong, fontWeight: '600' },
  { tag: t.emphasis, fontStyle: 'italic' },
  { tag: t.strikethrough, textDecoration: 'line-through' },
  { tag: [t.link, t.url], color: 'var(--cg-syn-function)', textDecoration: 'underline' },
  { tag: [t.deleted, t.invalid], color: 'var(--ui-error)' }
])

const mix = (c: string, p: number) => `color-mix(in oklab, ${c} ${p}%, transparent)`
const theme = EditorView.theme({
  '&': { height: '100%', color: 'var(--ui-text)', backgroundColor: 'var(--ui-bg)' },
  '&.cm-focused': { outline: 'none' },
  '.cm-scroller': { fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace', lineHeight: '1.6' },
  '.cm-content': { caretColor: 'var(--ui-secondary)', padding: '6px 0' },
  '.cm-cursor, .cm-dropCursor': { borderLeftColor: 'var(--ui-secondary)', borderLeftWidth: '2px' },
  '&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground': { backgroundColor: mix('var(--ui-secondary)', 28) },
  '.cm-selectionBackground': { backgroundColor: mix('var(--ui-secondary)', 18) },
  '.cm-gutters': { backgroundColor: 'var(--ui-bg-muted)', color: 'var(--ui-text-dimmed)', borderRight: '1px solid var(--ui-border)' },
  '.cm-lineNumbers .cm-gutterElement': { padding: '0 8px 0 12px' },
  '.cm-activeLine': { backgroundColor: mix('var(--ui-bg-accented)', 45) },
  '.cm-activeLineGutter': { backgroundColor: 'var(--ui-bg-elevated)', color: 'var(--ui-text-highlighted)' },
  '.cm-foldGutter .cm-gutterElement': { padding: '0 4px', cursor: 'pointer' },
  '.cm-matchingBracket, &.cm-focused .cm-matchingBracket': { backgroundColor: mix('var(--ui-success)', 22), outline: `1px solid ${mix('var(--ui-success)', 55)}` },
  '.cm-nonmatchingBracket, &.cm-focused .cm-nonmatchingBracket': { backgroundColor: mix('var(--ui-error)', 22) },
  '.cm-searchMatch': { backgroundColor: mix('var(--ui-warning)', 28), outline: `1px solid ${mix('var(--ui-warning)', 50)}` },
  '.cm-searchMatch.cm-searchMatch-selected': { backgroundColor: mix('var(--ui-warning)', 55) },
  '.cm-selectionMatch': { backgroundColor: mix('var(--ui-secondary)', 12) },
  '.cm-foldPlaceholder': { backgroundColor: 'var(--ui-bg-accented)', border: 'none', color: 'var(--ui-text-muted)', padding: '0 6px', borderRadius: '4px' },
  '.cm-specialChar': { color: 'var(--ui-error)' },
  '.cm-tooltip': { backgroundColor: 'var(--ui-bg)', color: 'var(--ui-text)', border: '1px solid var(--ui-border)', borderRadius: '6px', overflow: 'hidden' },
  '.cm-tooltip.cm-tooltip-autocomplete > ul': { fontFamily: 'inherit' },
  '.cm-tooltip-autocomplete ul li[aria-selected]': { backgroundColor: 'var(--ui-secondary)', color: 'var(--ui-bg)' },
  '.cm-panels': { backgroundColor: 'var(--ui-bg-muted)', color: 'var(--ui-text)' },
  '.cm-panels.cm-panels-top': { borderBottom: '1px solid var(--ui-border)' },
  '.cm-panels.cm-panels-bottom': { borderTop: '1px solid var(--ui-border)' },
  '.cm-panel.cm-search': { padding: '8px 36px 8px 12px', fontFamily: 'inherit' },
  '.cm-panel.cm-search label': { fontSize: '12px', color: 'var(--ui-text-muted)', whiteSpace: 'nowrap' },
  '.cm-panel.cm-search [name=close]': { top: '6px', right: '10px', fontSize: '20px', color: 'var(--ui-text-muted)' },
  '.cm-textfield': {
    backgroundColor: 'var(--ui-bg)', color: 'var(--ui-text)', border: '1px solid var(--ui-border-accented)',
    borderRadius: '6px', padding: '3px 8px', fontSize: 'inherit', margin: '2px 4px 2px 0'
  },
  '.cm-textfield:focus': { outline: '2px solid var(--ui-secondary)', outlineOffset: '-1px' },
  '.cm-button': {
    backgroundImage: 'none', backgroundColor: 'var(--ui-bg-elevated)', color: 'var(--ui-text)',
    border: '1px solid var(--ui-border)', borderRadius: '6px', padding: '3px 10px', margin: '2px 4px 2px 0', fontSize: '12px'
  },
  '.cm-button:hover': { backgroundColor: 'var(--ui-bg-accented)' },
  '.cm-button:active': { backgroundImage: 'none' }
})

function measure() {
  if (!view) return
  const st = view.state
  const head = st.selection.main.head
  const l = st.doc.lineAt(head)
  line.value = l.number
  col.value = head - l.from + 1
}
let sizeTimer: ReturnType<typeof setTimeout> | undefined
function measureSize() {
  clearTimeout(sizeTimer)
  sizeTimer = setTimeout(() => {
    if (view) bytes.value = new TextEncoder().encode(view.state.doc.toString()).length + (props.encoding === 'utf-8-bom' ? 3 : 0)
  }, 250)
}

async function doSave(): Promise<boolean> {
  if (!view || saving.value) return false
  const doc = view.state.doc
  saving.value = true
  try {
    if (!await props.save(doc.toString())) return false
    saved = doc
    dirty.value = !view.state.doc.eq(doc)
    return true
  } finally {
    saving.value = false
  }
}

function onKey(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && e.key.toLowerCase() === 's') {
    e.preventDefault()
    void doSave()
  }
}
useEventListener(window, 'keydown', onKey)

onMounted(async () => {
  const extensions: Extension[] = [
    lineNumbers(),
    highlightActiveLineGutter(),
    highlightSpecialChars(),
    history(),
    foldGutter(),
    drawSelection(),
    dropCursor(),
    EditorState.allowMultipleSelections.of(true),
    EditorState.phrases.of(phrases),
    indentOnInput(),
    syntaxHighlighting(highlight),
    bracketMatching(),
    closeBrackets(),
    autocompletion(),
    rectangularSelection(),
    crosshairCursor(),
    highlightActiveLine(),
    highlightSelectionMatches(),
    search({ top: true }),
    keymap.of([
      ...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap,
      ...foldKeymap, ...completionKeymap, indentWithTab
    ]),
    theme,
    languageConf.of([]),
    wrapConf.of([]),
    darkConf.of(EditorView.darkTheme.of(colorMode.value === 'dark')),
    EditorView.contentAttributes.of({ 'aria-label': props.filename }),
    EditorView.updateListener.of((u) => {
      if (u.selectionSet || u.docChanged) measure()
      if (u.docChanged) {
        dirty.value = !!saved && !u.state.doc.eq(saved)
        measureSize()
      }
    })
  ]
  view = new EditorView({
    parent: host.value!,
    state: EditorState.create({ doc: props.content, extensions })
  })
  saved = view.state.doc
  dirty.value = false
  measure()
  bytes.value = new TextEncoder().encode(props.content).length + (props.encoding === 'utf-8-bom' ? 3 : 0)
  view.focus()
  try {
    const support = await loadLanguage(lang)
    view?.dispatch({ effects: languageConf.reconfigure(support) })
  } catch {
    // A pack that failed to load (a stale chunk after an upgrade) leaves
    // plain text: the file is still editable.
  }
})

watch(() => colorMode.value, (mode) => {
  view?.dispatch({ effects: darkConf.reconfigure(EditorView.darkTheme.of(mode === 'dark')) })
})
watch(wrap, (on) => {
  view?.dispatch({ effects: wrapConf.reconfigure(on ? EditorView.lineWrapping : []) })
})

onBeforeUnmount(() => {
  clearTimeout(sizeTimer)
  view?.destroy()
  view = null
})

defineExpose({ save: doSave, saving })
</script>

<template>
  <div class="cg-code flex h-full min-h-0 flex-col">
    <div ref="host" class="min-h-0 flex-1 overflow-hidden" />
    <div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-default bg-muted px-4 py-1.5 text-xs text-muted">
      <span class="font-medium text-toned">{{ langLabel }}</span>
      <span class="tabular-nums">{{ tr('editor.position', { line, col }) }}</span>
      <span class="tabular-nums">{{ fmtBytes(bytes) }}</span>
      <span v-if="encoding">{{ encoding === 'utf-8-bom' ? 'UTF-8 BOM' : 'UTF-8' }}</span>
      <span class="hidden sm:inline">{{ tr('editor.shortcuts') }}</span>
      <USwitch v-model="wrap" size="xs" :label="tr('editor.wrap')" class="ms-auto" />
    </div>
  </div>
</template>

<style>
/* Syntax colours: the panel's palette, one set per colour mode. */
.cg-code {
  --cg-syn-keyword: var(--color-violet-700);
  --cg-syn-string: var(--color-emerald-700);
  --cg-syn-number: var(--color-amber-700);
  --cg-syn-comment: var(--cg-muted);
  --cg-syn-function: var(--cg-blue);
  --cg-syn-type: var(--cg-stamp);
  --cg-syn-property: var(--color-sky-700);
  --cg-syn-tag: var(--color-rose-700);
  --cg-syn-attribute: var(--color-amber-800);
  --cg-syn-variable: var(--color-cyan-800);
  --cg-syn-punctuation: var(--cg-muted);
  --cg-syn-meta: var(--cg-muted);
}
.dark .cg-code {
  --cg-syn-keyword: var(--color-violet-300);
  --cg-syn-string: var(--color-emerald-300);
  --cg-syn-number: var(--color-amber-300);
  --cg-syn-comment: var(--cg-muted);
  --cg-syn-function: var(--cg-blue);
  --cg-syn-type: var(--cg-stamp);
  --cg-syn-property: var(--color-sky-300);
  --cg-syn-tag: var(--color-rose-300);
  --cg-syn-attribute: var(--color-amber-200);
  --cg-syn-variable: var(--color-cyan-300);
  --cg-syn-punctuation: var(--cg-muted);
  --cg-syn-meta: var(--cg-muted);
}
.cg-code .cm-editor { font-size: 13px; }
/* Under 16px, iOS zooms the page when the editor takes focus. */
@media (pointer: coarse) {
  .cg-code .cm-editor, .cg-code .cm-textfield { font-size: 16px; }
}
</style>
