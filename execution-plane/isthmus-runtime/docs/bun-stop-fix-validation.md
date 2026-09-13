# Bun停机修复候选验证

调查日期：2026-09-13。状态：**Bun 1.4.2已在仓库外隔离验证通过；没有替换系统或项目版本**。用户本轮明确授权仅隔离下载/运行、不替换、不部署线上。系统及项目仍使用Bun 1.3.9，其异常停机对照仍失败；D4.1b不因候选通过而自动关闭。

## 证据链

- 本项目的`test/shutdown-regression.smoke.ts`在1.3.9中复现：服务端主动1011关闭后，原生stop不settle。已保留非0退出和`FAKE_SHUTDOWN_TIMEOUT`，不能用active=0宣称完成。
- [PR #32488](https://github.com/oven-sh/bun/pull/32488)及[commit 4bbe0751a2e5436757768325c2cc7ed97dc8767c](https://github.com/oven-sh/bun/commit/4bbe0751a2e5436757768325c2cc7ed97dc8767c)修复服务端关闭后的WebSocket计数结算，并补两种关闭方向测试；后续[PR #34346](https://github.com/oven-sh/bun/pull/34346)调整计数归属。
- 修复进入[1.4.0稳定版](https://github.com/oven-sh/bun/releases/tag/bun-v1.4.0)；候选采用[1.4.2官方发布](https://github.com/oven-sh/bun/releases/tag/bun-v1.4.2)。版本包含修复代码不等于本项目已兼容，尤其不能把issue的closed标签当本地回归结果。

GitHub commit compare的merge-base为上述修复SHA：
[fix→1.4.0](https://api.github.com/repos/oven-sh/bun/compare/4bbe0751a2e5436757768325c2cc7ed97dc8767c...bun-v1.4.0?per_page=1&page=2)和[fix→1.4.2](https://api.github.com/repos/oven-sh/bun/compare/4bbe0751a2e5436757768325c2cc7ed97dc8767c...bun-v1.4.2?per_page=1&page=2)的behind均为0；[1.3.14比较](https://api.github.com/repos/oven-sh/bun/compare/4bbe0751a2e5436757768325c2cc7ed97dc8767c...bun-v1.3.14?per_page=1&page=2)不含该修复。

本机arm64候选制品的[官方asset元数据](https://api.github.com/repos/oven-sh/bun/releases/assets/545329049)：`bun-darwin-aarch64.zip`，25,377,591字节，`digest`字段的SHA-256为：

```text
90987a3a16d7db556d886ac3d551e7b6d3edf0a1cf43acaed622e8676be1d12f
```

已重新读取官方JSON元数据并下载ZIP，实际大小及SHA-256均与上述元数据一致。压缩包只有目录和普通Mach-O arm64可执行文件，无软链接、路径穿越或额外条目；CRC亦通过。该比对不是代码安全审计或独立签名认证。

## 隔离验证边界

本轮获准后，把官方适合本机架构的制品放到仓库外全新0700私有工具目录，校验官方摘要及压缩包路径后才执行。未给全局PATH加目录、未运行远程安装脚本、未覆盖Homebrew Bun、未升级任何项目。所有测试用绝对路径选择候选版本。

按顺序验证（`/absolute/private/bun`为已审核的候选可执行文件路径）：

```sh
/absolute/private/bun --version
/absolute/private/bun --revision
make -C recovery check-demo BUN=/absolute/private/bun
make -C recovery runtime-shutdown-gate BUN=/absolute/private/bun
```

正常smoke和server-initiated close门槛必须分别exit 0；还要重复运行两类关闭路径，确认未靠sleep、超时成功或改变关闭码绕开问题。所有执行只针对新写fake代码与临时回环端口，不运行原始恢复包。

本次“不替换”的明确授权优先于此前的升级计划：即使验证通过，也不更新`package.json`、CI或系统版本。实际采用候选需另行确认；Linux x86_64与生产环境未验证。历史1.3.9失败证据保留，候选通过不批准上线、gRPC/CLI实现或生产数据迁移。

## 本轮实际验证

隔离目录：`/Users/ruanyang/My-project/api/z/sub2api-recovery-private.kbZovy/bun-1.4.2-validation.If2HP3`。
二进制：其下`bun-darwin-aarch64/bun`；`--version`为`1.4.2`，`--revision`为`1.4.2+744846f84`，大小61,884,464字节，SHA-256为`35d20dd0263e5c950194434b925454fdfa9ba6e4467da960410fa05b08a7a5b5`。

| 实际执行 | 结果 |
| --- | --- |
| `make -C recovery check-demo BUN=<隔离二进制绝对路径>` | exit 0：85项Python、111项Bun、33项React，Go演示7包race/vet及命令编译、typecheck/build、真实正常回环HTTP/WS与停止均通过 |
| `make -C recovery runtime-shutdown-gate BUN=<隔离二进制绝对路径>` | exit 0：收到1011、START/CHUNK且无END之后，真正await原生stop成功 |
| 候选正常关闭重复验证 | 5个独立进程均exit 0，同时验证transport与shutdown结果 |
| 候选1011异常关闭重复验证 | 5个独立进程均exit 0；未改关闭码、deadline或测试代码 |
| `/opt/homebrew/bin/bun`运行同一异常probe对照 | exit 1，仍复现1.3.9停机失败；没有把此失败算成门槛通过 |
| 无替换检查 | 系统Bun二进制及入口软链接、项目package.json、两份恢复CI和Makefile的SHA-256与验证前相同 |
| 新代码/旧WIP保护 | 写入本节记录前，与`workspace-evidence-final`逐项比较255份元数据/文件均未变；只新增隔离下载与被忽略的测试构建产物 |
| 技能离线自测 | `web-reverse-master`全部7项通过；原始恢复包未执行 |

两个probe已独立只读审查：timeout只会拒绝，不会伪装成功。正常probe在finally可能单独打印shutdown PASS，因此上述验收以进程exit 0且transport/shutdown两项同时通过为准，而不是仅搜索PASS文字。
