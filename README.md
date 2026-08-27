# edu-agent

`edu-agent` 是单用户自托管的通用个人学习助理。Go 服务和 PostgreSQL 保存知识、教学状态、学习事件与投影；Go CLI 是当前客户端；Nocturne sidecar 保存经过准入的长期个人记忆。

## 开发入口

开始需求设计、实现、测试或验收前，先阅读：

1. [`docs/development/README.md`](docs/development/README.md)
2. 当前 Comet change 的 brief/spec
3. 当前 capability 的 `docs/design/` 文档

项目采用垂直交付、分层测试和证据复用。用户可见需求变化通过 Comet Native 返回 Shape，不能只在实现或测试中隐式修改。

## 常用命令

```bash
make test
make vet
make build
make check
```

真实 PostgreSQL 测试需要显式配置 `TEST_DATABASE_URL`，未配置时的 skip 不代表数据库行为通过。CLI 和服务端分别位于 `clients/cli-go` 与 `server`。
