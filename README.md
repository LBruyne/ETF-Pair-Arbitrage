# ETF Pair Arbitrage

交易 SOXL / SOXS 永续的均值回归策略（首版只做 Bitget，用户 2026-11-23）。交易程序在 `core/` 里按实现设计 [core/SPEC.md](core/SPEC.md)（0.8，经外部审计五次批复）分步实现：到 2026-11-25，第 0–4 步（脚手架、落库、行情与锚点、策略计算、库存账本）已实现并通过测试，第 5 步（订单、纸面撮合、Bitget 账户侧）实施中；执行循环、看板和回放还没有，任何模式下都不发真实订单。2026-11-19 用户另行授权的 **Bitget Python实盘试验**已按用户要求结束，11月19日18:49（UTC+8）整体盈利平仓、11月20日再次确认零持仓；实现于 `live_pair/`，规则与操作见 [本次试验](docs/LIVE_BITGET_20261119.md)，不能等同于完整 SPEC 已交付。

## 当前在运行（2026-11-23，UTC+8）

| 服务 | 做什么 | 文档 |
|---|---|---|
| `etf-pair-pairscan.service`（采集器 `pairscan`） | 两所合约的分钟价、USD/USDT、ETF 锚点（铃锚点后 NAV）、分红拆股及其核对、交易日历，写入正式 PostgreSQL 的 `etfpair` 库 | [dashboard/SPEC.md](dashboard/SPEC.md) 第 3、4 节 |
| `etf-pair-booktape.service`（录制服务 `booktape`） | 两所 WebSocket 的盘口与成交：每个推送原样存进 `experiments/book_tape/`，每个合约每秒一行写进 `etfpair.perp_second` | [dashboard/SPEC.md](dashboard/SPEC.md) 附录 A |
| Combo Spread 看板 3.3（本机共享 Grafana，`/dashboards/arbitrage/etf-pair/combo-spread`；2026-11-23 13:54 发布） | 按 Session 画两个组合在两所的每秒价差（两腿中价）、均线、带符号的偏离（Deviation 图）、按每笔名义金额 N 逐档吃两腿盘口的 Real Spread 判断的信号和入场点（Entry Points 图，N 可在看板上设置）、两腿前两档的深度、锚点和分红拆股 | [dashboard/SPEC.md](dashboard/SPEC.md)、[dashboard/README.md](dashboard/README.md) |

每张表、每份文件由谁写、从何时起、多大，以及已删除数据的存档：[DATA.md](DATA.md)。

## 当前文档

- [PRD.md](PRD.md)：交易程序唯一的规则文档（2026-11-24 按用户要求从零重写，现为 3.3）：范围、数据、价差与均线、开仓、额度、仓位与记账、平仓、流程、参数、回测要求和验收；标【取法】的是 lead 自己定的地方。
- 根目录的 SPEC.md（交易程序的架构设计）已于 2026-11-24 按用户要求删除，最后的副本在 `experiments/decisions_20261123/prd_rewrite_20261124/before/`，只作历史。新的实现设计是 [core/SPEC.md](core/SPEC.md)（2026-11-24，用户第 41 条；交易程序的模块、接口、算法的参考代码、表和实施步骤；Live Monitor 看板的设计在 dashboard/SPEC.md 第 7 节），审计 2026-11-24 已看过 0.3 并批复，0.4 按批复修订；PRD 现为 3.6（1.4 节列出首版的收窄）。
- [dashboard/SPEC.md](dashboard/SPEC.md)、[dashboard/README.md](dashboard/README.md)：`Arbitrage/ETF-Pair` 看板、采集器和录制服务的设计与运维。Combo Spread 已实现并发布；Live Monitor 仍是设计。改看板、采集器、录制服务或它们的表要同步更新这两份文件。
- [DATA.md](DATA.md)：数据清单。
- [SOURCES.md](SOURCES.md)：在用的外部来源、读取节奏和授权；历史核验记录。
- [RESEARCH.md](RESEARCH.md)：研究盘点、结论边界、清理清单与可复核证据。
- [.codex/memory/MEMORY.md](.codex/memory/MEMORY.md)：项目当前记忆索引。

锚点（用户 2026-11-20）：每个 Session 先用收盘时的铃锚点，发行方 NAV 稳定后用 NAV；分红、拆股按生效时刻调整，节假日和临时停市进入日历；交易按两腿的实时盘口判断（PRD 第 3 节）。只在非盘中开仓，盘中只平仓，常规收盘前 1 分钟全部平掉；均线（EMA 或 SMA）和 σ 每个 Session 重新开始（PRD 第 4、5、10、12 节）。开仓要两个条件：可执行偏离过了 d_in·σ 的开仓线，并且开平仓线的距离盖过往返手续费；盘口按 0.01 归档成有效档位后取第 2 档的价；每一批开仓是一个仓位，分别平仓、先开先平记账（PRD 第 5、7、9 节；用户 2026-11-23、11-24）。

2026-11-17 用户确认生产首版的实施范围（PRD 1.3 节）：只做双 taker、不按 R04 加仓（2026-11-23 起同一周期可分批吃单，合计最多 N）、允许有条件反向开单；`d`、杠杆、均线等参数数值后续提供；代码目录参照相邻套利项目；看板建在 Grafana 的 `Arbitrage/ETF-Pair` 目录。

## 当前代码

| 路径 | 能做什么 | 不能据此声称什么 |
|---|---|---|
| `dashboard/` | 已部署的采集器 `pairscan`、录制服务 `booktape`、看板生成器，核对（`audit/`）与回补（`backfill/`）脚本、部署脚本。 | 不是交易程序；交易程序不引用它的包、不读它的库，锚点、分红拆股和日历自己从同一批来源读（PRD 3.3，用户 2026-11-22）。 |
| `live_pair/` | 11-19 已结束的 Python 双 taker 试验源码（目录里另有其他研究的入口，不属于本项目的交易程序）。 | 不可按原样重启，不代表生产交易程序完成。 |
| `research/` | 留下的是录制服务数据的读取工具 `research/booktape.py`（看板的审计和回补导入它）、均线对照实现 `rollover.py` 及其依赖、`account_snapshot.py` （目录里另有其他研究的脚本，不属于交易程序）；历史研究和一次性核验脚本 2026-11-22 移到各自的实验目录（RESEARCH.md 第 13 节）。 | 不作为生产实现；不能把模拟成交称为真实成交。 |
| `docs/research/` | 新研究方法与报告、标明历史状态的原报告和旧 SPEC。 | 各次研究范围单列，不自动替换生产PRD。 |
| `experiments/` | 原始行情、录制文件、账本、来源响应及本地复核材料（不入库）。 | 不作为当前运行状态或已获利润证明。 |

2026-11-20 删除了半成品 Go 代码（`pilot/`、`cmd/pair-live`、`tape/`、`cmd/flow-record`、`strategy/`；源码存档见 DATA.md 第 3 节），交易程序从头实现。实时数据、账户费率与当前余额不从历史文件推断。

## 离线验证与复核

从本目录运行：

```bash
go build ./...
go vet ./...
go test -race ./...
python3 dashboard/grafana/test_boards.py
.venv/bin/python -I -m unittest discover -s dashboard/backfill -p 'test_*.py'
.venv/bin/python -m unittest research.test_booktape research.test_rollover   # the two references the trading program uses
```

看板、采集器和录制服务的检查（连库的测试、`recompute.py`、`sources.py`、`audit/booktape.py`）见 [dashboard/README.md](dashboard/README.md)。Python 依赖见 [requirements.txt](requirements.txt)。独立恢复历史环境时再创建 `.venv`。`experiments/go.mod` 只隔离冻结的旧 Go 源码副本，避免根目录测试把历史副本当生产包；它不是新的运行模块，不需要单独构建。

`research/account_snapshot.py` 仅从外部私有配置读取凭据并发 GET，不复制凭据，不修改账户。不要将私人配置、原始行情、交易账本、二进制或图表输出提交。

后续：用户检查 PRD → 回测选参 → 写实现设计 → 实现与离线测试 → 纸面验收 → 经授权的小额实盘（PRD 第 16–18 节）。提交、部署和真实下单分别按用户授权进行。
