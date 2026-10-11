import type { Extension } from '@codemirror/state'
import { StreamLanguage } from '@codemirror/language'
import type { EditorLang } from '~/utils/fileTypes'

/**
 * Loads the CodeMirror support for one language. Every pack is a dynamic
 * import, so each becomes its own chunk, fetched the first time a file of
 * that language is opened.
 */
export async function loadLanguage(lang: EditorLang): Promise<Extension> {
  switch (lang) {
    case 'php': return (await import('@codemirror/lang-php')).php()
    case 'javascript': return (await import('@codemirror/lang-javascript')).javascript()
    case 'jsx': return (await import('@codemirror/lang-javascript')).javascript({ jsx: true })
    case 'typescript': return (await import('@codemirror/lang-javascript')).javascript({ typescript: true })
    case 'tsx': return (await import('@codemirror/lang-javascript')).javascript({ jsx: true, typescript: true })
    case 'css': return (await import('@codemirror/lang-css')).css()
    case 'scss': return (await import('@codemirror/lang-sass')).sass()
    case 'sass': return (await import('@codemirror/lang-sass')).sass({ indented: true })
    case 'less': return (await import('@codemirror/lang-less')).less()
    case 'html': return (await import('@codemirror/lang-html')).html()
    case 'json': return (await import('@codemirror/lang-json')).json()
    case 'yaml': return (await import('@codemirror/lang-yaml')).yaml()
    case 'sql': {
      const m = await import('@codemirror/lang-sql')
      return m.sql({ dialect: m.MySQL })
    }
    case 'markdown': return (await import('@codemirror/lang-markdown')).markdown()
    case 'xml': return (await import('@codemirror/lang-xml')).xml()
    case 'python': return (await import('@codemirror/lang-python')).python()
    case 'shell': return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/shell')).shell)
    case 'nginx': return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/nginx')).nginx)
    case 'properties': return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/properties')).properties)
    case 'toml': return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/toml')).toml)
    case 'dockerfile': return StreamLanguage.define((await import('@codemirror/legacy-modes/mode/dockerfile')).dockerFile)
    default: return []
  }
}
