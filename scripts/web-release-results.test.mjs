import { test } from 'node:test'
import assert from 'node:assert/strict'
import { checkGoResults, checkBrowserResults } from './web-release-results.mjs'

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
  ]) assert.throws(() => checkBrowserResults(report(item), 'firefox'))
  assert.throws(() => checkBrowserResults({ suites: [] }, 'firefox'))
  assert.throws(() => checkBrowserResults({ ...report(passed()), errors: [{ message: '服务启动失败' }] }, 'firefox'))
})
