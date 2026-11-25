import { existsSync } from 'node:fs'
import { isDeepStrictEqual } from 'node:util'
import { assert, fileHash, fromRoot, keysExactly, readJSON, sameSet, verifyArtifacts } from './audit-lib.mjs'
import { registryFor, roles } from './reviews.mjs'

function reviewedMeasurement(root, review, payload) {
  assert(Array.isArray(review.measurements) && review.measurements.length > 0, 'No native applicability measurement')
  sameSet(review.measurements.map(item => item.target), review.measurements.map(item => item.target), 'applicability measurement targets')
  const measurement = review.measurements.find(item => item.target === payload.target)
  assert(measurement, 'No applicability measurement for current native target')
  keysExactly(measurement, ['target', 'path', 'sha256'], 'applicability measurement')
  const evidence = { path: measurement.path, sha256: measurement.sha256 }
  verifyArtifacts(root, [evidence])
  const measured = readJSON(fromRoot(root, measurement.path))
  assert(measured.version === 1 && measured.target === payload.target && measured.test_exit === 0 && Array.isArray(measured.files),
    'Invalid native applicability measurement')
  const old = measured.files.filter(file => file.path === review.path)
  const current = payload.files.filter(file => file.path === review.path)
  assert(old.length === 1 && current.length === 1, 'Applicability measurement omitted or duplicated its source')
  for (const file of [old[0], current[0]]) {
    assert(file.active === true && Array.isArray(file.sites) && file.sites.length > 0 &&
      file.sites.every(site => site.kind === 'line' && site.hit === true) &&
      Array.isArray(file.unmeasured_expression_lines) && file.unmeasured_expression_lines.length === 0,
    'Only active fully measured straight-line source can have branch N/A')
  }
  const points = file => file.sites.map(({ line, column, kind, hit }) => ({ line, column, kind, hit }))
  assert(isDeepStrictEqual(points(old[0]), points(current[0])), 'Current measurement differs from reviewed executable source')
  return evidence
}

function checkedReview(root, reference, registry, payload, expectedPaths) {
  const review = readJSON(fromRoot(root, reference.path))
  keysExactly(review, ['format_version', 'kind', 'session_id', 'role', 'path', 'source_sha256', 'metric',
    'disposition', 'reason', 'measurements'], 'coverage applicability review')
  assert(review.format_version === 1 && review.kind === 'coverage-branch-applicability' && review.metric === 'branch' &&
    review.disposition === 'NOT_APPLICABLE' && typeof review.reason === 'string' && review.reason.trim(), 'Unsupported coverage disposition')
  assert(expectedPaths.includes(review.path) && fileHash(fromRoot(root, review.path)) === review.source_sha256,
    'Applicability source is absent or changed')
  assert(roles.includes(review.role) && !registry.implementer_ids.includes(review.session_id) &&
    registry.sessions.some(session => session.session_id === review.session_id && session.role === review.role),
  'Applicability reviewer is not an actual independent registered role')
  return { review, evidence: reviewedMeasurement(root, review, payload) }
}

// A genuine zero is N/A only after independent source and native measurement
// review. Every file and every executable line remains in the inventory.
export function reviewedBranchApplicability(root, payload, expectedPaths, registryBinding) {
  const indexPath = 'compliance/coverage-applicability.json'
  if (!existsSync(fromRoot(root, indexPath))) return { paths: [], artifacts: [], dispositions: [] }
  assert(registryBinding?.path === 'compliance/review-session-registry.json', 'No frozen applicability registry binding')
  const registry = registryFor(root, registryBinding, true)
  const index = readJSON(fromRoot(root, indexPath))
  keysExactly(index, ['format_version', 'reviews'], 'coverage applicability index')
  assert(index.format_version === 1 && Array.isArray(index.reviews), 'Malformed coverage applicability index')
  verifyArtifacts(root, index.reviews)
  const byPath = new Map()
  const artifacts = [{ path: indexPath, sha256: fileHash(fromRoot(root, indexPath)) }, registryBinding, ...index.reviews]
  for (const reference of index.reviews) {
    const { review, evidence } = checkedReview(root, reference, registry, payload, expectedPaths)
    artifacts.push(evidence)
    const group = byPath.get(review.path) ?? []
    group.push(review)
    byPath.set(review.path, group)
  }
  for (const group of byPath.values()) {
    sameSet(group.map(review => review.role), roles, 'independent applicability axes')
    sameSet(group.map(review => review.session_id), group.map(review => review.session_id), 'independent applicability identities')
  }
  return {
    paths: [...byPath.keys()],
    artifacts: [...new Map(artifacts.map(artifact => [artifact.path, artifact])).values()],
    dispositions: [...byPath].map(([path, reviews]) => ({
      path, metric: 'branch', disposition: 'NOT_APPLICABLE', source_sha256: reviews[0].source_sha256,
      reviewers: reviews.map(({ role, session_id }) => ({ role, session_id })),
      reason: 'Genuine zero outcomes, independently reviewed source and target-native measurement; file and executable lines remain measured.',
    })),
  }
}
