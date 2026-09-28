# himind-extensions-ecc

把 [affaan-m/ECC](https://github.com/affaan-m/ECC) 的技能库，按固定策略搬成 HiMind Agent 能装的能力包，并自动发到市场和 GitHub。

同一份上游提交，跑多少次生成结果都一模一样——所以「上游没变」可以被判定成零动作，不会产生无意义的版本号和提交。

## 里面有什么

| 目录 | 说明 |
| --- | --- |
| `skills/` | 迁移后的技能，正文与上游逐字一致，只补了 `skill.json` |
| `plugins/ecc-skill-sync/` | 同步插件，5 个能力：探测 / 拉取 / 生成 / 门禁 / 发布 |
| `workflows/ecc-skill-sync/` | 把这 5 步串起来的工作流，可手动跑，也可挂定时计划 |
| `tools/cmd/himind-ecc-sync/` | 同一个过程的命令行入口，调试用 |
| `internal/eccsync/` | 真正的实现，插件和 CLI 共用 |
| `upstream-policy.json` | 迁移策略：搬什么、不搬什么、怎么改名 |
| `upstream.lock.json` | 同步锚点：当前对齐到上游哪个提交 |
| `manifests/` | 元数据、隔离清单、模块使用情况 |
| `.himind/catalog.json` | 市场索引，发布时写入 |
| `extensions.json` | 仓库清单，每条扩展显式声明分发目标 |

## 日常怎么用

在 HiMind Agent 里跑「ECC 技能同步」工作流即可，默认走全流程：探测 → 拉取 → 生成 → 门禁 → 发布。上游没动，整条链会在第一步就结束。

想定时，就把它注册成定时计划（每天一次足够，例如 `0 3 * * *`）。跑完的结果在任务中心能查到。

命令行调试：

```powershell
go run ./tools/cmd/himind-ecc-sync probe    -repo-root .
go run ./tools/cmd/himind-ecc-sync generate -repo-root .
go run ./tools/cmd/himind-ecc-sync gate     -repo-root .
go run ./tools/cmd/himind-ecc-sync publish  -repo-root . -dry-run
```

## 自动化的边界

没人看着的时候，只允许这一类变化自动发版：只动了 A 档技能、门禁全绿、元数据已人工校对过。其余情况一律停下等人——B 档改写、技能被删或改名、上游许可证变化、门禁不过。

停下来不是失败：门禁不过会把原因写进 `manifests/quarantine.json`，元数据没校对过的技能会进「待校对」队列，等人在 `manifests/metadata.json` 里把 `source` 改成 `reviewed` 才会发出去。

细节和取舍写在 [docs/ADR.md](docs/ADR.md)。

## 许可

`skills/` 的内容来自 ECC，遵循 MIT，版权归 Affaan Mustafa，见 [LICENSE-ECC](LICENSE-ECC)；来源说明见 [NOTICE](NOTICE)。仓库其余部分由本仓库作者维护。
