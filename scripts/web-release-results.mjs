// 发布检查只接受实际执行成功的测试；退出 0 本身不能证明没有 skip。
export function checkGoResults(output, required = []) {
  const events = output.trim().split('\n').filter(Boolean).map(line => JSON.parse(line))
  const tests = events.filter(event => event.Test && ['pass', 'fail', 'skip'].includes(event.Action))
  if (!tests.length || tests.some(event => event.Action !== 'pass') || events.some(event => event.Action === 'fail') ||
    required.some(name => !tests.some(event => event.Test === name && event.Action === 'pass')))
    throw new Error('Go 检查缺少必需用例、失败或存在跳过项')
  return tests.length
}

export function checkBrowserResults(report, project) {
  const tests = []
  function visit(suites) {
    for (const suite of suites) {
      for (const spec of suite.specs ?? []) tests.push(...spec.tests)
      visit(suite.suites ?? [])
    }
  }
  visit(report.suites ?? [])
  if (report.errors?.length || !tests.length || tests.some(test =>
    test.projectName !== project || test.status !== 'expected' || test.expectedStatus !== 'passed' ||
    test.results?.length !== 1 || test.results[0].status !== 'passed'))
    throw new Error('浏览器检查没有用例、失败、意外失败预期、重试或跳过项')
  return tests.length
}
