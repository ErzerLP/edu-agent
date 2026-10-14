// Named requirements prevent a deleted file or an empty/renamed selection from
// silently shrinking C1–C5 evidence. Other browser specs are still enumerated.
export const requiredBrowserScenarios = {
  'learner.spec.ts': ['学习端全权一次配对完成文本导入、Studio 笔记与记忆候选，设置和清除权限独立'],
  'notebook-chat.spec.ts': [
    'C5a 网页概要显示标题与正文摘要，截断查看器给出短提示',
    '来源行打开原文，菜单预算仍用于对话提交',
    'C2 方向、空对话、引用、实时改选、方向修订、保存重开与 Studio 笔记',
    '旧学习区方向只由区名预填，经确认后才能发送',
    '新建学习区立即添加来源只打开一次',
    '归档学习区不接受会被忽略的方向编辑，恢复后可修改方向',
  ],
  'notebook-exercise.spec.ts': [
    'C3b 出题、帮助、丢响应核对、自动反馈、引用、收起恢复与进度',
    'C3b 零来源只给入口，待复核不显示通过',
  ],
  'notebook-studio.spec.ts': [
    '所选来源生成指南和简报、引用定位、刷新重开、取消并重新生成版本',
    'C4 无手选目标生成测验、共享卡片作答与反馈、改方向后继续旧题组',
    'Studio 拒绝缺失 CSRF 的写入，读取撤权后清除尚未保存的草稿',
    '笔记保存重开、冲突保留草稿，已保存版本转来源后不随笔记修改',
    '笔记草稿在标签页和学习区之间隔离，保存丢响应后核对原操作',
  ],
  'notebook-sources.spec.ts': [
    '搜索发现只发送明确确认的关键词，选中后读取正文并经原预览确认成为来源',
    '搜索取消、失败和空候选如实显示，修改关键词使原外发确认失效',
    'Notebook 三种添加方式保留真实来源、部分覆盖和原操作核对，查看器定位历史片段',
  ],
  'notebook-workspace.spec.ts': [
    '配对、新建学习区、卡片进入、建立并恢复真实课堂、正式答案与反馈',
    '390 与 1280 宽度无整页溢出，底部标签切换保留对话草稿',
    '同标签页切区恢复草稿，双标签页互不覆盖，来源不跨区',
    '菜单完整可达、键盘关闭返回焦点、旧深链接与无效 query 不扩大范围',
    '来源分页目录保持同名文档归属，读取权限失效立即隐藏目录',
    '归档学习区禁止新建目标与课堂，恢复后目标区可继续写入',
    '切区后来源目录的迟到响应不进入新区，也不覆盖新区对话草稿',
    '所选来源问答：无目标、改选、核验引用、零来源、双标签与原操作核对',
  ],
  'notebook-accessibility.spec.ts': [
    'C5a 五宽度双主题、来源和 Studio、短视口与减少动态效果',
    'C5a 键盘菜单、来源、对话引用返回、练习反馈与 Studio 全流程',
    'C5a 练习与反馈卡五宽度双主题',
    'C5a 长标题、公式与代码留在窄屏消息内',
  ],
  'release.spec.ts': ['发行页面直达、刷新、真实资产与错误路由边界'],
}

export function releaseBrowserFiles(files) {
  const specs = files.filter(file => file.endsWith('.spec.ts')).sort()
  const missing = Object.keys(requiredBrowserScenarios).filter(file => !specs.includes(file))
  if (missing.length) throw new Error(`浏览器门禁缺少必需文件：${missing.join('、')}`)
  return specs
}

export function browserSpecPattern(file) {
  return `tests/browser/${file.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`
}

export function browserFixtures(file, binaries = {}) {
  const model = !['learning.spec.ts', 'settings.spec.ts'].includes(file)
  if (file === 'notebook-sources.spec.ts' && !binaries.notebook)
    throw new Error('来源验收缺少隔离网络宿主')
  if (file === 'review.spec.ts' && (!binaries.review || !binaries.cli))
    throw new Error('复习验收缺少独立夹具或生产 CLI')
  return {
    WEB_RELEASE_MATRIX: '1',
    WEB_WORKSPACE_FIXTURE: model ? '1' : '0',
    WEB_MENTOR_FIXTURE: model ? '1' : '0',
    WEB_NOTESYNC_FIXTURE: file === 'notesync.spec.ts' ? '1' : '0',
    WEB_NOCTURNE_FIXTURE: file === 'memory.spec.ts' ? '1' : '0',
    ...(file === 'notebook-sources.spec.ts' ? { WEB_NOTEBOOK_FIXTURE_BINARY: binaries.notebook } : {}),
    ...(file === 'review.spec.ts' ? { WEB_REVIEW_FIXTURE: binaries.review, EDU_AGENT_INTEGRATION_CLI: binaries.cli } : {}),
  }
}

export const notebookPostgresPackages = [
  './internal/transport/httpapi', './internal/identity/postgresstore',
  './internal/learningspace/postgresstore', './internal/learning/postgresstore', './internal/mentorrun',
]
export const requiredNotebookPostgres = [
  'TestPostgreSQLLearnerAdminIssuanceAndHTTPPermissionMatrix',
  'TestPostgreSQLWebLearnerScopesPersistWithoutUpgradingOldDevice',
  'TestPostgreSQLDirectionFailureRollsBackSpaceAndReceipt',
  'TestPostgreSQLDirectionCreateAndLegacyCompatibility',
  'TestPostgreSQLDirectionConcurrentEnsureAndRevision',
  'TestPostgreSQLDirectionLegacyReceiptDoesNotAcquireNewIntent',
  'TestPostgreSQLStudyModeIsExplicitAndSaved',
  'TestPostgreSQLStudySourceAndDirectionIsolationReopen',
  'TestPostgreSQLStudyCitationRetriesStayBounded',
  'TestPostgreSQLStudyDirectionErasureCannotRevive',
  'TestPostgreSQLStudyExerciseAssessmentResumeProjection',
  'TestPostgreSQLStudyExerciseDeferSubmitFeedback',
  'TestPostgreSQLStudyExerciseAtomicFailureAndZeroSources',
  'TestPostgreSQLStudyExerciseOldContextAndRestart',
  'TestPostgreSQLStudyExerciseResumeConflictsAndRevocation',
  'TestPostgreSQLStudyExercisePublishesOneCard',
  'TestPostgreSQLStudyExerciseHelpSubmissionConcurrency',
  'TestPostgreSQLStudyExerciseClearCannotRevive',
  'TestPostgreSQLExerciseSubmissionAndHelp',
  'TestPostgreSQLExerciseAutomaticAssessment',
  'TestPostgreSQLExerciseLeaseRecovery',
  'TestPostgreSQLExerciseOriginalDirectionAndSources',
  'TestPostgreSQLExerciseProvisionalAndAllowedHelp',
  'TestPostgreSQLExerciseTaskFailureRollsBackAttempt',
  'TestPostgreSQLExerciseHelpCompetesWithSubmission',
  'TestPostgreSQLExercisePublicationRollbackAndHelpState',
  'TestPostgreSQLExerciseRecoveryChecksAssessmentOperation',
  'TestPostgreSQLExerciseSessionActionRechecksSubmitter',
  'TestPostgreSQLExerciseKnownRejectionIsNotUnknown',
  'TestPostgreSQLExerciseBudgetAndInternalPorts',
  'TestPostgreSQLExerciseRevocationAndRotation',
  'TestPostgreSQLExerciseGlobalErasureCancelsInFlight',
  'TestPostgreSQLExerciseConfigurationChangeRequiresExplicitResume',
  'TestPostgreSQLStudioDirectionFrozenQuizAndAutomaticFeedback',
  'TestPostgreSQLStudioExerciseAssessmentRecovery',
  'TestPostgreSQLStudioExerciseSharedReadRetainsAssessmentOwner',
  'TestPostgreSQLStudioExerciseScopeRevocationAndClear',
  'TestPostgreSQLStudioLegacyFirstQuestionVersionConflict',
  'TestPostgreSQLStudioQuizFormalSessions',
  'TestPostgreSQLProgressScopePaginationAndReplay',
  'TestPostgreSQLReviewCarrierPreservesSourceAndReplay',
]
// Explicit names avoid selecting opt-in live-provider tests and counting their skips.
export const notebookPostgresPattern = `^(${requiredNotebookPostgres.join('|')})$`
