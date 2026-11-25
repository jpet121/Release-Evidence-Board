import { Buffer } from 'node:buffer'
import { FAIL, assert, coverage, sameSet } from './audit-lib.mjs'

export const templateRangeCases = [
  ['guarded-empty', 1, [[false, false]]], ['guarded-one', 1, [[true, true]]], ['guarded-many', 1, [[true, true]]],
  ['plain-empty', 2, [[false, true]]], ['plain-one', 2, [[true, true]]], ['plain-many', 2, [[true, true]]],
  ['break-one', 3, [[true, false]]], ['break-many', 3, [[true, false]]],
  ['continue-one', 4, [[true, true]]], ['continue-many', 4, [[true, true]]],
  ['nested-break', 5, [[true, true], [true, false]]],
  ['explicit-empty', 6, [[false, true, true]]], ['explicit-one', 6, [[true, false, true]]],
  ['error-partial', 7, [[true, false]]],
  ['emptybody-empty', 8, [[false, true]]], ['emptybody-many', 8, [[true, true]]],
  ['nestedexplicit-break', 9, [[true, true], [true, false, true]]],
  ['nestedexplicit-empty', 9, [[true, true], [true, true, true]]],
  ['outerbreak-stop', 10, [[true, false], [true, true]]],
  ['conditionalbreak-stop', 11, [[true, false]]], ['conditionalfinish-natural', 11, [[true, true]]],
]
export const templateRangeControlIDs = templateRangeCases.map(([name]) => 'coverage-template-range-' + name)
export const templateRangePaths = ['render.go', 'view.html']
export const templateRangeProducerPaths = [
  'compliance/tools/covercheck.go', 'compliance/tools/templatecheck.go',
  ...['go.mod', 'render.go', 'render_test.go', 'view.html'].map(name => 'compliance/fixtures/template-range/' + name),
]
export const templateRangeDefinition = 'Go and embedded html/template AST execution and outcome probes, including typed Go continuation lines, each evaluated supported grouped expression switch alternative, and template short-circuit operands. AC-024-06 owner-delegated alternative: implicit-else template ranges measure iteration and natural exhaustion; explicit-else ranges retain nonempty/empty outcomes. All source remains inventoried with zero exclusions. Native statement profiles are supplemental. Original delivered probe totals retain their original metric and outcome identities.'

export function checkTemplateRangeVector(raw, target, name, source, bounds) {
  const selected = templateRangeCases.find(row => row[0] === name)
  assert(selected, 'Unknown template range qualification selector')
  assert(raw.target === target && raw.test_exit === 0, 'Template range vector target/exit differs')
  sameSet(raw.files.map(file => file.path), templateRangePaths, 'independent template range fixture inventory')
  const expected = new Map()
  const shapes = new Map(templateRangeCases.map(([, line, pairs]) => [line, pairs.map(pair => Boolean(pair[2]))]))
  const lines = source.split(/\r?\n/)
  for (const [line, explicit] of shapes) {
    const columns = [...lines[line - 1].matchAll(/\{\{range /g)].map(match => Buffer.byteLength(lines[line - 1].slice(0, match.index + 8)) + 1)
    assert(columns.length === explicit.length, 'Range fixture source differs from oracle')
    explicit.forEach((hasElse, index) => {
      const kinds = hasElse ? ['template-range:true', 'template-range:false'] : ['template-range:iteration', 'template-range:exhausted']
      kinds.forEach((kind, outcome) => expected.set(JSON.stringify([line, columns[index], kind]), line === selected[1] ? selected[2][index][outcome] : false))
    })
  }
  const sites = raw.files.find(file => file.path === 'view.html').sites.filter(site => site.kind.startsWith('template-range:'))
  const key = site => JSON.stringify([site.line, site.column, site.kind])
  sameSet(sites.map(key), [...expected.keys()], 'complete range probe inventory')
  for (const site of sites) assert(site.hit === expected.get(key(site)), 'Unexpected range outcome: ' + name + ' ' + key(site))
  const parsed = coverage(raw, target, templateRangePaths, bounds, ['render.go'])
  assert(parsed.status === FAIL && parsed.observations.files.some(file => file.missed_sites.length > 0),
    'Deliberately incomplete range fixture was promoted to passing coverage')
  return sites.filter(site => site.line === selected[1])
}
