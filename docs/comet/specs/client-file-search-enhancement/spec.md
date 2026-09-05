# Capability: 文件内容检索增强

结果：已有search支持更有用的呈现和可选忽略规则，在保持兼容与边界的前提下降低项目检索噪声。

## 合同与验收

1. 保留query/path/mode/case/include/exclude的现有默认与语义；mode仍为literal/regex，case仍支持smart等现有值。
2. 新增output=content/files/count，默认content；context为0..3默认0，非法/未知字段拒绝。context非零只允许content模式。
3. content返回匹配行及有界邻近行，实际行号明确，重叠邻近区域不重复膨胀；超长行UTF-8安全裁剪。
4. files只列包含匹配的路径（每文件一次）；count返回已扫描部分的匹配行和匹配文件统计；不完整时必须标明局部计数，不能冒充全目录总数。
5. 新glob使用find明确的递归语法；旧include/exclude继续Go path.Match，不静默重解释。新glob与旧include共同收窄，exclude优先。
6. search/find新增respect_gitignore=false；缺省不改旧行为。true时只加载工作区内根及逐层.gitignore，不读外部祖先、全局配置或执行git。
7. 支持基本Git忽略匹配、注释、目录规则、逐层覆盖与合法否定；被整体排除目录不深入，不能假称支持无法遍历的重新包含。忽略文件读取/解析有界且不跟随链接；限制或失败明确标注不完整/错误，不静默扩大范围。
8. 显式文件作为search范围不因目录发现过滤而隐藏；归档特殊规则独立，普通范围跳过、显式范围只读允许。忽略只是筛选，不是权限。
9. 保留现有扫描文件/字节/入口/深度/结果预算与取消；新增上下文或计数不能隐式放大资源。统计与截断原因诚实。
10. 具名测试覆盖三种output、上下文合并、旧兼容、递归glob、根/嵌套.gitignore、否定、链接、归档、局部count以及生产投影。

延期：外部rg/fd、全局忽略配置、索引、全仓无限统计、多行正则。
