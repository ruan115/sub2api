# Source Baseline

## Repository

- Repository: `/Users/ruanyang/My-project/api/z/sub2api`
- Source branch: `ccmax`
- Source commit: `45183bc52d1e160b3d9a5a5a7e4afb0118a29e43`
- Source subject: `fix(ccmax): validate final Anthropic request shape`
- Development branch: `codex/claude-execution-plane-v1`
- PRD: `docs/prd/claude-execution-plane-v1.md`

创建分支时仅存在已确认的 PRD 和用于跟踪该 PRD 的 `.gitignore` 精确白名单，没有业务代码修改。

## Existing Behavior To Preserve

- CCMAX 账号、分组、代理、Session Key/OAuth/Setup Token 授权。
- RPM/ITPM、会话粘性、capacity queue、429/529、冷缓存和计费。
- `/v1/messages`、`/v1/messages/count_tokens`、models 和 `/v1/chat/completions`。
- 原始请求透传、Claude Code 兼容转换、分组开关与错误脱敏。
- 账号统计、归档、审计和独立 `ccmax-manager/web` 管理界面。

## Local Tooling

- macOS arm64；Go `1.26.0`；Node `20.20.2`；内置 pnpm `11.19.0`。
- 仓库当前 overrides 布局与 pnpm 11 不兼容；冻结基线使用一次性固定的 pnpm `10.34.5`，未修改 package/lockfile。
- `protoc`、全局 `buf` 和 OpenSpec CLI 当前未安装；execution-plane 已固定 Buf CLI `v1.72.0` 和 remote plugin 版本，通过 `go run` 复现，不依赖开发机隐式版本。

## Baseline Verification

| Command | Result | Notes |
|---|---|---|
| `cd ccmax-manager && go test ./...` | PASS | 2026-08-29，缓存命中 |
| Frontend lockfile supply-chain policy | PASS | 1018 entries |
| `npx --yes pnpm@10.34.5 install --frozen-lockfile` | PASS | 976 packages；package/lockfile 未变化 |
| `npx --yes pnpm@10.34.5 run typecheck` | PASS | `vue-tsc --noEmit` |
| `npx --yes pnpm@10.34.5 run test:run` | BASELINE FAIL | 229/230 files、1629/1630 tests 通过；既有 `CreateAccountModal.grok.spec.ts` 对 `? 'xai-...'` 的源码断言失败 |

上述唯一 Vitest 失败存在于本次业务代码修改之前，且 execution-plane 未修改前端源码；作为后续 UI 阶段的已知基线差异保留，不在本阶段顺手修改。

## First Worker Node Inventory

- SSH alias: `newapi-txy`
- Address: `43.172.83.39`
- Ubuntu 22.04.5 LTS amd64；腾讯云 KVM CVM；4 vCPU；约 8 GiB RAM；120 GB disk；无 swap。
- Docker/containerd/podman 当前未安装；LXD socket-activated 但未运行。
- 80/443 已有监听；UFW active；部署不得覆盖现有服务。
- 试点硬上限：20 slots、4 active CLI、12 oauth_api requests。
- 在单独部署批准前只允许只读检查。
