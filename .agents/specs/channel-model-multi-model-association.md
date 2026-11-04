# channel_model 多模型关联(channel_model multi-model association)

适用范围:`internal/objects/model.go`、`internal/server/biz/model_association_matcher.go`、`internal/server/biz/model_settings_inheritance.go`、`internal/server/orchestrator/candidates.go`、`frontend/src/features/models/**`。

## 背景与决策

- 2026-11-04 Heavy 任务:模型关联 `channel_model`(渠道内精准匹配模型)从单 `modelId` 改为多 `modelIds`(用户拍板路线 B 后端支持;范围仅 channel_model,`model` / `channel_tags_model` 保持单选)。
- 上游 v1.0.0-beta12 未实装,为 fork 自有实现。

## 稳定约定

1. **存储契约**:`ChannelModelAssociation = { ChannelID int, ModelIDs []string }`,JSON 键 `modelIds`。读路径兼容旧 `modelId` 单值:自定义 `UnmarshalJSON` 在 `modelIds` 为空且 `modelId` 非空时归一为单元素数组;两者并存时 `modelIds` 优先。Marshal 只输出 `modelIds`(旧数据下次保存时自动迁移,无需 SQL migration)。
2. **匹配语义**:matcher 按 `ModelIDs` 顺序在 `GetModelEntries()`(键为 RequestModel)逐个查找,按 `(channelID, modelID)` tracker 去重,命中多个进入同一 connection 的 `Models`;全部缺失则该规则无连接(静默跳过,不报错)。
3. **开发者设置**:写系统设置时清空 `ModelIDs = nil`;继承时填充 `[]string{modelID}`。缓存签名用 `writeStringSliceSignature`(nil 与空切片签名相同,不会误失效)。
4. **GraphQL**:`model.graphql` 中 type 与 input 均为 `modelIds: [String!]!`;gqlgen 绑定不变。**注意**:仓库 HEAD 上 `go generate` 因 `dashboard.resolvers.go` 存量问题(follow-schema 重写丢私有辅助函数)本来就会失败,与 schema 改动无关;单独运行 `cd internal/server/gql && go run ../gqlgen/generate.go` 可生成,生成后 `git checkout` 还原被误改的 resolver 文件,并核对 generated.go 两次生成 sha256 一致。
5. **前端多选控件**:`TagsAutocompleteInput` 新增 `strict` / `emptyMessage` props(默认 false,不影响渠道标签等自由输入场景)。strict 模式仅接受建议列表中的精确(大小写不敏感)匹配,回车/逗号/失焦不会把未确认搜索词写入值;渠道变更时必须清空 `modelIds`(在 channelId 的 onSelectedValueChange 中同步 setValue,不用 useEffect)。
6. **已接受风险**:绕过 UI 直接调 GraphQL 可写入空 `modelIds` / 未知渠道,matcher 会安全跳过,不加服务端校验(管理员 API,过度防御)。

## 派发路径备忘(不入 journal)

情报检索员(Grok)→ 后端(后端架构执行者/Command Code)∥ 前端(前端执行者/Antigravity)→ 红队(黑客红队/Grok,无阻塞发现,2 条前端建议已返工修复)→ code-simplifier(移除 TagsAutocompleteInput 未使用的 disabled prop、合并 guidance memo)。头脑风暴伙伴两次调用异常中断,决策门改为直接询问用户完成。
