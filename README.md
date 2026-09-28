# himind-extensions-ecc

把 [affaan-m/ECC](https://github.com/affaan-m/ECC) 的技能库，按固定策略搬成 HiMind Agent 能装的能力包，并自动发到市场和 GitHub。

同一份上游提交，跑多少次生成结果都一模一样——所以「上游没变」可以被判定成零动作，不会产生无意义的版本号和提交。

## 里面有什么

| 目录 | 说明 |
| --- | --- |
| `skills/` | 迁移后的技能，正文与上游逐字一致，只补了 `skill.json` |
| `plugins/ecc-skill-sync/` | 同步插件，8 个能力：探测 / 拉取 / 生成 / 门禁 / 发布 / 校对队列 / 分发状态 / 分发策略 |
| `plugins/ecc-skill-sync/ui/` | 插件自带的分发管理界面，按分类与模块控制谁参与分发 |
| `workflows/ecc-skill-sync/` | 把这 6 步串起来的工作流，可手动跑，也可挂定时计划 |
| `tools/cmd/himind-ecc-sync/` | 同一个过程的命令行入口，调试用 |
| `internal/eccsync/` | 真正的实现，插件和 CLI 共用 |
| `upstream-policy.json` | 迁移策略：搬什么、不搬什么、怎么改名 |
| `dispatch-policy.json` | 分发策略：搬进来的技能里，哪一类不往外发 |
| `upstream.lock.json` | 同步锚点：当前对齐到上游哪个提交 |
| `manifests/` | 元数据、隔离清单、模块使用情况 |
| `.himind/catalog.json` | 市场索引，发布时写入 |
| `extensions.json` | 仓库清单，每条扩展显式声明分发目标 |

## 日常怎么用

在 HiMind Agent 里跑「ECC 技能同步」工作流即可，默认走全流程：探测 → 拉取 → 生成 → 门禁 → 发布 → 报出待校对。上游没动，整条链会在第一步就结束。

最后一步是给「技能为什么少了几条」准备的：机械文案没经人读过就上不了市场，这是有意的闸门，但闸门得会报数。收尾会把还欠校对的条数写进运行记录（未校对 / 待重校分开算），不用回头翻仓库才知道该补哪一批。

入口只要求一个参数：**同步仓库工作区**（`workspace_root`，正常填本仓库根目录）。这条链要拉上游源码、写生成结果、跑门禁，必须落在明确目录里，所以缺工作区会在入口就被拒，不会跑到一半才失败。

注意：定时计划的 `input` 也要写 `workspace_root`（不是 `repo_root`）。入口声明的是 `requires: ["workspace"]`，只认 `workspace_root` / `workspace` / `project_root` 这三个别名；写别的名字会在调度器启动 Run 时被入口判定拦下（`workflow entrypoint sync requires seed artifact or verifiable fact: workspace`）。插件本身两种名字都收，但入口判定先跑。

想定时，就把它注册成定时计划（每天一次足够，例如 `0 3 * * *`）。跑完的结果在任务中心能查到。

### 自动跑起来的三段

| 段 | 怎么发生 | 看哪里 |
| --- | --- | --- |
| 探测 → 发布 | 工作流跑 `probe → fetch → generate → gate → publish`，够格就发 Release | GitHub Release（tag 形如 `workflow/com.mrbaoquan.workflow.ecc-skill-sync@1.0.1`）、`.himind/catalog.json` |
| 装回来 | 用 `extension.distribution.install` 带上 `repository` / `tag` / `id` / `version` | 安装报告的 `installed` 字段；装完 `current_version` 会更新，旧版本留在 `versions` 里可回滚 |
| 到点触发 | 定时计划按 cron 拉起同一条 Run，输入里带 `workspace_root` | `schedule.list` 的 `last_run_id` / `last_status` / `next_run_at` |

安装判定是**版本精确匹配**：本机装着 1.0.0 时去装 1.0.1 会正常走升级，不会因为「同名扩展已存在」被跳过。

已发布的工作流按 `plugin → skill → workflow` 顺序进索引——工作流依赖里 pin 了插件的版本与摘要，插件没先进索引，工作流的 pin 就解析不出来。

无人值守的两条硬要求，都已经写进实现里：

- **网络抖动不该让当天的同步整条失败。** 所有 GitHub 请求（REST / raw / codeload / `gh`）共用一套有界重试（5 次，指数退避带正向抖动、单次等待上限 30 秒，只重试传输错误、5xx、429）；用尽后在错误里标明重试次数。
- **上游事实只取一次，随源码树落盘。** `fetch` 把本次对齐的提交、时间、版本写进源码树根的 `.ecc-upstream-facts.json`，`generate` 只读本地、不再联网问 HEAD。这样 HEAD 在两次探测之间移动也不会让「同一提交生成同一份字节」失效。
- **命中缓存就不再联网。** `probe` 刚落下的上游提交与版本记在 `.cache/ecc-sync/last-probe.json`（30 分钟内有效），`fetch` 优先读它；本地已有该提交的源码树时，整步零网络请求。

发布同样可以断点续跑：先查 tag 上的 Release 是否存在，缺资产就补传，两个资产都回读确认之后才写索引。半成品状态重跑会自愈，不会撞上「同名 tag 已存在」。

命令行调试：

```powershell
go run ./tools/cmd/himind-ecc-sync probe    -repo-root .
go run ./tools/cmd/himind-ecc-sync generate -repo-root .
go run ./tools/cmd/himind-ecc-sync gate     -repo-root .
go run ./tools/cmd/himind-ecc-sync publish  -repo-root . -dry-run
go run ./tools/cmd/himind-ecc-sync review-queue -repo-root . -format md
go run ./tools/cmd/himind-ecc-sync review-apply -repo-root . -decisions .tmp/decisions.json
```

插件的二进制是提交进仓库的（Agent 侧直接拉起它，不现场编译）。改完插件或 `internal/eccsync` 必须重建，否则装了新版本的机器跑的还是旧行为：

```powershell
go build -o plugins/ecc-skill-sync/ecc-skill-sync.exe ./plugins/ecc-skill-sync
```

## 谁不往外发：分发策略

上游 268 条技能不会条条都用得上，所以「往外发」这件事得能收窄。`upstream-policy.json` 决定「哪些上游技能值得搬进仓库」，`dispatch-policy.json` 决定「已经搬进来的技能里，哪些继续往外发」——两份合成一份的话，「不想发」会被记成「不想收」，技能从仓库里消失，用户既看不见也回不来。

策略按三类规则生效，越具体的先判：单条技能 > 上游模块 > HiMind 分类。缺省是「全部参与分发」，所以三个集合都是空表时，跑出来的结果和没有这份文件时一样。

被排掉的技能不再发新版本。线上已有的旧版本不会下架（下架对用户是倒退），只是从这一刻起停止更新。本次跳过多少条会写进发布报告的 `excluded` 与 `excluded_items`，记的是「少了哪一条、被分类 / 模块 / 单条哪条规则挡的、为什么」——半年后有人问某条技能怎么不更新了，答案在报告和策略文件里。

发布报告每次跑完都落一份，位置是 `.cache/ecc-sync/reports/<UTC 时间戳>-publish.json`，和生成报告（`-generate.json`）并排：生成报告说这次对齐到哪个上游提交，发布报告说在这个提交上谁发了、谁被挡下。定时任务没人看着，这份文件就是事后唯一能翻的账。三个入口（`himind-ecc-sync publish`、`ecc.sync.publish` 能力、定时工作流）写的是同一份报告，手动发版同样留痕。

插件与工作流不受这份策略约束：它们是本仓库自己写的扩展，要停发改的是自己的分发落点；把两者混在一起，哪天误排一个分类，工具链自己就跟着停更了。

三种改法，读的都是仓库里这一份文件：

| 方式 | 怎么做 |
| --- | --- |
| 界面 | 插件视图「分发管理」：按分类或模块整组开关，单条技能也能单独排掉，改完即落盘 |
| 命令行 | `dispatch` 看状态，`dispatch-exclude` / `dispatch-include` 改策略 |
| 定时任务 | 什么都不用做：`ecc.sync.publish` 每次读同一份策略，排掉的技能一直跳过，跳过清单落进当天的发布报告 |

```powershell
# 看此刻每条技能卡在哪一步（distributed / pending / held / excluded）
go run ./tools/cmd/himind-ecc-sync dispatch -repo-root . -state excluded

# 整块排掉一个上游模块，写清原因
go run ./tools/cmd/himind-ecc-sync dispatch-exclude -repo-root . -exclude-module media-generation -reason "团队不做视频生成"

# 反悔了，整块放回来
go run ./tools/cmd/himind-ecc-sync dispatch-include -repo-root . -include-module media-generation
```

模块名、分类名、技能 slug 写错会在落盘前被拦下——这种错如果不拦，不会报错，只会静默地什么都不排除，人以为排掉了、实际还在发。

策略是内容事实，改完要提交到仓库：定时任务跑在哪个工作区，读的就是那个工作区里的这一份文件。只在某台机器上改、不提交，别的机器和下一次干净检出都会回到旧策略。

## 自动化的边界

没人看着的时候，只允许这一类变化自动发版：只动了 A 档技能、门禁全绿、元数据已人工校对过。其余情况一律停下等人——B 档改写、技能被删或改名、上游许可证变化、门禁不过。

停下来不是失败：门禁不过会把原因写进 `manifests/quarantine.json`，元数据没校对过的技能会进「待校对」队列，等人在 `manifests/metadata.json` 里把 `source` 改成 `reviewed` 才会发出去。

校对队列不必手翻元数据表：

```powershell
# 还欠着人工校对的技能（--status stale 只看「上游改了正文、文案该重读」的）
go run ./tools/cmd/himind-ecc-sync review-queue -repo-root . -format md

# 把一轮校对结论落库，一次读一批
go run ./tools/cmd/himind-ecc-sync review-apply -repo-root . -decisions .tmp/decisions.json
```

`decisions.json` 的形状是 `{"技能目录名": {"name": "中文动作短语（≤18 字）", "description": "一句话说清用途（≤120 字）"}}`。落库是**先全批校验、再整体写**：一条不合法就一条都不落，避免人读到一半被打断却不知道停在哪里。

校对过的技能会记一个**校对基线**：只有人重新改过文案时，基线才跟到当前正文；上游单方面改了正文，这条会进 `stale` 队列提示重读，但不会撤下市场。

细节和取舍写在 [docs/ADR.md](docs/ADR.md)。

## 许可

`skills/` 的内容来自 ECC，遵循 MIT，版权归 Affaan Mustafa，见 [LICENSE-ECC](LICENSE-ECC)；来源说明见 [NOTICE](NOTICE)。仓库其余部分由本仓库作者维护。
