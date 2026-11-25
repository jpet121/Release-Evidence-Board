import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { assert, fileHash, sha256 } from './audit-lib.mjs'

// Independent source inventory, not the list claimed by a browser report.
export function cssScope(root, inputs, allowEmpty=false) {
  const documents = [], segments = {}
  for (const entry of inputs.source) {
    if (!/\.(css|html)$/.test(entry.path)) continue
    assert(fileHash(join(root, entry.path)) === entry.sha256, 'CSS inventory document changed')
    const text = readFileSync(join(root, entry.path), 'utf8').replace(/\r\n?/g, '\n')
    if (entry.path.endsWith('.css')) segments[entry.path] = sha256(text)
    else {
      const styles = [...text.matchAll(/<style\b[^>]*>([\s\S]*?)<\/style\s*>/gi)]
      for (const [index, style] of styles.entries()) segments[entry.path+'#style-'+index] = sha256(style[1])
      if (!styles.length) continue
    }
    documents.push(entry)
  }
  assert(allowEmpty || documents.length > 0, 'No first-party CSS scope')
  return {method:'reb-css-application/1', documents, segments}
}
