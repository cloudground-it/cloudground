/**
 * What the file manager knows about a file from its name alone: the editor
 * language for text, or the kind of a file that is never opened as text.
 * Kept free of imports so it stays in the main bundle; the language packs
 * themselves are loaded by ~/lib/editorLanguages only when the editor opens.
 */

/** The editor largest file: the agent's maxEditableFile (2 MiB). */
export const MAX_EDITABLE_BYTES = 2 * 1024 * 1024

export type EditorLang =
  | 'php' | 'javascript' | 'jsx' | 'typescript' | 'tsx'
  | 'css' | 'scss' | 'sass' | 'less' | 'html' | 'json' | 'yaml' | 'sql'
  | 'markdown' | 'xml' | 'python' | 'shell' | 'nginx' | 'properties' | 'toml'
  | 'dockerfile' | 'text'

export type BinaryKind = 'image' | 'archive' | 'font' | 'media' | 'document' | 'binary'

export interface FileKind {
  /** Editor language; absent for a file known to be binary. */
  lang?: EditorLang
  /** The language's display name (for text). */
  label: string
  binary?: BinaryKind
  /** MIME type of an image the file card can preview. */
  imageType?: string
  icon: string
}

type Lang = [EditorLang, string]

const byName: Record<string, Lang> = {
  '.htaccess': ['shell', 'Apache'],
  'nginx.conf': ['nginx', 'Nginx'],
  'dockerfile': ['dockerfile', 'Dockerfile'],
  'containerfile': ['dockerfile', 'Dockerfile'],
  '.env': ['properties', 'Env'],
  '.user.ini': ['properties', 'INI'],
  '.editorconfig': ['properties', 'INI'],
  '.npmrc': ['properties', 'INI'],
  '.bashrc': ['shell', 'Shell'],
  '.bash_profile': ['shell', 'Shell'],
  '.profile': ['shell', 'Shell'],
  '.zshrc': ['shell', 'Shell'],
  '.babelrc': ['json', 'JSON'],
  '.eslintrc': ['json', 'JSON'],
  '.prettierrc': ['json', 'JSON'],
  'composer.lock': ['json', 'JSON']
}

const byExt: Record<string, Lang> = {
  php: ['php', 'PHP'], phtml: ['php', 'PHP'], php5: ['php', 'PHP'], php7: ['php', 'PHP'], php8: ['php', 'PHP'], inc: ['php', 'PHP'],
  js: ['javascript', 'JavaScript'], mjs: ['javascript', 'JavaScript'], cjs: ['javascript', 'JavaScript'],
  jsx: ['jsx', 'JSX'], ts: ['typescript', 'TypeScript'], mts: ['typescript', 'TypeScript'], cts: ['typescript', 'TypeScript'], tsx: ['tsx', 'TSX'],
  css: ['css', 'CSS'], scss: ['scss', 'SCSS'], sass: ['sass', 'Sass'], less: ['less', 'Less'],
  html: ['html', 'HTML'], htm: ['html', 'HTML'], xhtml: ['html', 'HTML'], vue: ['html', 'Vue'], twig: ['html', 'Twig'],
  json: ['json', 'JSON'], jsonc: ['json', 'JSON'], map: ['json', 'JSON'], webmanifest: ['json', 'JSON'],
  yml: ['yaml', 'YAML'], yaml: ['yaml', 'YAML'], neon: ['yaml', 'NEON'],
  sql: ['sql', 'SQL (MySQL)'],
  md: ['markdown', 'Markdown'], markdown: ['markdown', 'Markdown'],
  xml: ['xml', 'XML'], svg: ['xml', 'SVG'], xsd: ['xml', 'XML'], xsl: ['xml', 'XML'], rss: ['xml', 'XML'], atom: ['xml', 'XML'], plist: ['xml', 'XML'], xliff: ['xml', 'XML'],
  py: ['python', 'Python'],
  sh: ['shell', 'Shell'], bash: ['shell', 'Shell'], zsh: ['shell', 'Shell'], ksh: ['shell', 'Shell'],
  conf: ['nginx', 'Nginx'], nginx: ['nginx', 'Nginx'],
  ini: ['properties', 'INI'], cnf: ['properties', 'INI'], cfg: ['properties', 'INI'], properties: ['properties', 'Properties'], env: ['properties', 'Env'],
  toml: ['toml', 'TOML'], dockerfile: ['dockerfile', 'Dockerfile']
}

const images: Record<string, string> = {
  png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp',
  avif: 'image/avif', ico: 'image/x-icon', bmp: 'image/bmp'
}
const binaryExt: Record<string, BinaryKind> = {
  tif: 'image', tiff: 'image', heic: 'image', psd: 'image',
  zip: 'archive', gz: 'archive', tgz: 'archive', tar: 'archive', bz2: 'archive', xz: 'archive', '7z': 'archive', rar: 'archive', zst: 'archive', wpress: 'archive', phar: 'archive', jar: 'archive',
  woff: 'font', woff2: 'font', ttf: 'font', otf: 'font', eot: 'font',
  mp3: 'media', mp4: 'media', m4a: 'media', m4v: 'media', webm: 'media', ogg: 'media', oga: 'media', ogv: 'media', wav: 'media', flac: 'media', mov: 'media', avi: 'media', mkv: 'media',
  pdf: 'document', doc: 'document', docx: 'document', xls: 'document', xlsx: 'document', ppt: 'document', pptx: 'document', odt: 'document', ods: 'document', odp: 'document',
  exe: 'binary', bin: 'binary', so: 'binary', dll: 'binary', o: 'binary', a: 'binary', class: 'binary', mo: 'binary', sqlite: 'binary', db: 'binary', dat: 'binary', swf: 'binary'
}
const binaryIcons: Record<BinaryKind, string> = {
  image: 'i-cg-image',
  archive: 'i-cg-archive',
  font: 'i-cg-file-text',
  media: 'i-cg-play',
  document: 'i-cg-file-text',
  binary: 'i-cg-file'
}

/** The file's extension, lower case, without the dot ("" if none). */
export function fileExt(name: string): string {
  const base = name.toLowerCase()
  const i = base.lastIndexOf('.')
  return i > 0 ? base.slice(i + 1) : ''
}

function textKind([lang, label]: Lang): FileKind {
  const icon = lang === 'text' ? 'i-cg-file-text' : lang === 'markdown' ? 'i-cg-file-text' : 'i-cg-file-code'
  return { lang, label, icon }
}

/** Classifies a file by its name. */
export function detectFile(name: string): FileKind {
  let base = name.toLowerCase()
  // A template keeps its real type: .env.example, phpunit.xml.dist…
  base = base.replace(/\.(dist|example|sample|orig|bak|template|tpl)$/, '') || base
  const named = byName[base]
  if (named) return textKind(named)
  if (base.startsWith('.env')) return textKind(['properties', 'Env'])
  if (base.startsWith('dockerfile') || base.endsWith('.dockerfile')) return textKind(['dockerfile', 'Dockerfile'])
  if (base.endsWith('.blade.php')) return textKind(['html', 'Blade'])
  if (base.endsWith('.html.twig')) return textKind(['html', 'Twig'])
  const ext = fileExt(base)
  const known = byExt[ext]
  if (known) return textKind(known)
  const imageType = images[ext]
  if (imageType) return { label: ext.toUpperCase(), binary: 'image', imageType, icon: binaryIcons.image }
  const kind = binaryExt[ext]
  if (kind) return { label: ext.toUpperCase(), binary: kind, icon: binaryIcons[kind] }
  return textKind(['text', 'Plain text'])
}

/** A language from a script's first line (#!/usr/bin/env python3…). */
export function detectShebang(firstLine: string): Lang | null {
  if (!firstLine.startsWith('#!')) return null
  if (/\b(ba|z|k|da)?sh\b/.test(firstLine)) return ['shell', 'Shell']
  if (/\bpython[0-9.]*\b/.test(firstLine)) return ['python', 'Python']
  if (/\bphp\b/.test(firstLine)) return ['php', 'PHP']
  if (/\bnode\b/.test(firstLine)) return ['javascript', 'JavaScript']
  return null
}
