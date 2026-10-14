import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checkGoResults, checkBrowserResults } from './web-release-results.mjs'
import { browserFixtures, releaseBrowserFiles, requiredBrowserScenarios, requiredNotebookPostgres, notebookPostgresPattern } from './web-release-scenarios.mjs'
import * as scenarios from './web-release-scenarios.mjs'

test('Go 跳过、空选择和失败不能作为发布证据', () => {
  const event = Action => JSON.stringify({ Action, Test: 'TestPostgreSQLExample' })
  assert.equal(checkGoResults(event('pass')), 1)
  assert.throws(() => checkGoResults(event('pass'), ['TestPostgreSQLStudyCLIResearchAndWebContent']))
  assert.throws(() => checkGoResults(event('pass') + '\n' + JSON.stringify({ Action: 'fail', Package: 'example' })))
  for (const output of ['', JSON.stringify({ Action: 'pass', Package: 'example' }), event('skip'), event('fail'), event('pass') + '\n' + event('skip')])
    assert.throws(() => checkGoResults(output))
})

const passed = () => ({ projectName: 'firefox', status: 'expected', expectedStatus: 'passed', results: [{ status: 'passed' }] })
const report = value => ({ suites: [{ suites: [{ specs: [{ tests: [value] }] }] }], errors: [] })
test('浏览器必须在指定引擎上执行成功，不能用期望失败、skip 或重试冒充', () => {
  assert.equal(checkBrowserResults(report(passed()), 'firefox'), 1)
  for (const item of [
    { ...passed(), projectName: 'chromium' },
    { ...passed(), expectedStatus: 'failed', results: [{ status: 'failed' }] },
    { ...passed(), status: 'skipped', results: [{ status: 'skipped' }] },
    { ...passed(), results: [] },
    { ...passed(), results: [{ status: 'failed' }, { status: 'passed' }] },
    { ...passed(), results: [{ status: 'passed', retry: 1 }] },
  ]) assert.throws(() => checkBrowserResults(report(item), 'firefox'))
  assert.throws(() => checkBrowserResults({ suites: [] }, 'firefox'))
  assert.throws(() => checkBrowserResults({ ...report(passed()), errors: [{ message: '服务启动失败' }] }, 'firefox'))
})

test('浏览器报告必须实际执行本文件的必需场景，空选或改名不能悄悄通过', () => {
  const value = report(passed())
  value.suites[0].suites[0].specs[0].title = '方向与来源隔离'
  assert.equal(checkBrowserResults(value, 'firefox', ['方向与来源隔离']), 1)
  assert.throws(() => checkBrowserResults(value, 'firefox', ['学习端权限隔离']))
  value.suites[0].suites[0].specs.push({ title: '学习端权限隔离', tests: [] })
  assert.throws(() => checkBrowserResults(value, 'firefox', ['学习端权限隔离']))
})

test('候选选择保留新增 Notebook 文件和其夹具，缺失必需文件即拒绝', () => {
  const files = [...Object.keys(requiredBrowserScenarios), 'future-feature.spec.ts', 'fixture.ts']
  assert.ok(releaseBrowserFiles(files).includes('future-feature.spec.ts'))
  assert.ok(!releaseBrowserFiles(files).includes('fixture.ts'))
  assert.throws(() => releaseBrowserFiles(files.filter(file => file !== 'learner.spec.ts')))
  assert.ok(requiredBrowserScenarios['notebook-accessibility.spec.ts'].includes('C5a 练习与反馈卡五宽度双主题'))
  assert.deepEqual(browserFixtures('notebook-exercise.spec.ts'), {
    WEB_RELEASE_MATRIX: '1', WEB_WORKSPACE_FIXTURE: '1', WEB_MENTOR_FIXTURE: '1', WEB_NOTESYNC_FIXTURE: '0', WEB_NOCTURNE_FIXTURE: '0',
  })
  assert.equal(browserFixtures('memory.spec.ts').WEB_NOCTURNE_FIXTURE, '1')
  assert.equal(browserFixtures('notesync.spec.ts').WEB_NOTESYNC_FIXTURE, '1')
  assert.equal(browserFixtures('learning.spec.ts').WEB_WORKSPACE_FIXTURE, '0')
  assert.equal(browserFixtures('settings.spec.ts').WEB_MENTOR_FIXTURE, '0')
})

test('Notebook PostgreSQL 选择覆盖权限、方向、练习与 Studio，真实模型需另验', () => {
  const selected = new RegExp(notebookPostgresPattern)
  for (const name of ['TestPostgreSQLLearnerAdminIssuanceAndHTTPPermissionMatrix', 'TestPostgreSQLDirectionConcurrentEnsureAndRevision', 'TestPostgreSQLStudyModeIsExplicitAndSaved', 'TestPostgreSQLStudyCitationRetriesStayBounded', 'TestPostgreSQLStudyExerciseHelpSubmissionConcurrency', 'TestPostgreSQLExerciseRevocationAndRotation', 'TestPostgreSQLStudioDirectionFrozenQuizAndAutomaticFeedback', 'TestPostgreSQLStudioLegacyFirstQuestionVersionConflict']) assert.ok(selected.test(name), name)
  assert.ok(!selected.test('TestPostgreSQLStudyExerciseLiveResponses'))
  assert.ok(!selected.test('TestPostgreSQLStudioDirectionLiveResponses'))
  const output = requiredNotebookPostgres.map(Test => JSON.stringify({ Action: 'pass', Test })).join('\n')
  assert.equal(checkGoResults(output, requiredNotebookPostgres), requiredNotebookPostgres.length)
  assert.throws(() => checkGoResults(output.replace('"pass"', '"skip"'), requiredNotebookPostgres))
  assert.throws(() => checkGoResults(output.split('\n').slice(1).join('\n'), requiredNotebookPostgres))
})

test('来源和复习浏览器选择必须带专用宿主与生产 CLI，不能靠 skip', () => {
  const binaries = { notebook: '/tmp/webnotebook', review: '/tmp/webreview', cli: '/tmp/edu-agent' }
  assert.equal(browserFixtures('notebook-sources.spec.ts', binaries).WEB_NOTEBOOK_FIXTURE_BINARY, '/tmp/webnotebook')
  assert.equal(browserFixtures('review.spec.ts', binaries).WEB_REVIEW_FIXTURE, '/tmp/webreview')
  assert.equal(browserFixtures('review.spec.ts', binaries).EDU_AGENT_INTEGRATION_CLI, '/tmp/edu-agent')
  assert.throws(() => browserFixtures('notebook-sources.spec.ts'))
  assert.throws(() => browserFixtures('review.spec.ts', { review: binaries.review }))
})

test('按 spec 精确选择，workspace 不会重跑 notebook-workspace', () => {
  const pattern = new RegExp(scenarios.browserSpecPattern('workspace.spec.ts'))
  assert.ok(pattern.test('/repo/clients/web/tests/browser/workspace.spec.ts'))
  assert.ok(!pattern.test('/repo/clients/web/tests/browser/notebook-workspace.spec.ts'))
  assert.ok(!pattern.test('/repo/clients/web/tests/browser/workspaceXspecXts'))
})
