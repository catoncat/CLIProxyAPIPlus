# Codex Transport Benchmark

用于比较 `Codex -> CLIProxy` 在 `HTTP` 和 `WebSocket v2` 两种 transport 下的实际表现。

脚本入口：[`codex_transport_bench.py`](/Users/envvar/P/CLIProxyAPIPlus/scripts/bench/codex_transport_bench.py)

重点看三类指标：

- 端到端耗时
- token 消耗
- `WebSocket v2` 多轮场景的缓存命中和 telemetry

## 前置条件

- 本地 `cli-proxy` 已启动，且 `/v1/responses` 可访问
- 已准备代理 API key，例如 `key1`
- 如需分组级 usage 交叉校验，已准备 management password，例如 `abc123!!`
- 本机可运行 `uv`
- 如需跑 `Codex CLI` 黑盒组，本机已安装 `codex`

先确认服务状态：

```bash
scripts/cliproxy.sh status
```

## 脚本做什么

脚本分两层：

- `raw`：直接打代理的 `/v1/responses`，得到最干净的 transport 基线
- `cli`：调用本机 `codex exec --json`，观察真实 CLI 端到端行为

默认行为：

- `raw` 默认跑 `multi_turn`
- `cli` 默认跑单轮黑盒
- 每组都会对比 `http` 和 `ws`
- 有 management password 时，会在组级别抓 `/v0/management/usage` 做 token 交叉校验

## 关键参数

- `--base-url`：代理地址。可以传 `http://localhost:8317`，也可以直接传 `http://localhost:8317/v1`
- `--api-key`：代理 API key，例如 `key1`
- `--management-key`：management password，例如 `abc123!!`
- `--model`：测试模型，默认 `gpt-5.3-codex`
- `--samples`：每组轮数，默认 `3`
- `--raw-scenarios`：逗号分隔的 raw 场景，支持 `single_turn,multi_turn`
- `--skip-raw`：跳过 raw 组
- `--skip-cli`：跳过 Codex CLI 组
- `--codex-bin`：Codex CLI 路径，默认自动找 `codex`
- `--cli-provider`：Codex 配置里的 provider 名，默认 `local`
- `--cli-workdir`：CLI 黑盒运行目录；不传则自动建一个临时空目录，减少 repo/AGENTS 噪音
- `--output-dir`：结果根目录，脚本会自动创建时间戳子目录
- `--log-file`：显式指定 `main.log` 路径
- `--verbose`：打印更多环境信息

## 典型命令

只跑 raw 多轮基线：

```bash
uv run scripts/bench/codex_transport_bench.py \
  --base-url http://localhost:8317 \
  --api-key key1 \
  --management-key 'abc123!!' \
  --model gpt-5.3-codex \
  --samples 5 \
  --skip-cli
```

只跑 Codex CLI 黑盒：

```bash
uv run scripts/bench/codex_transport_bench.py \
  --base-url http://localhost:8317 \
  --api-key key1 \
  --management-key 'abc123!!' \
  --model gpt-5.3-codex \
  --samples 5 \
  --skip-raw
```

一次跑全套：

```bash
uv run scripts/bench/codex_transport_bench.py \
  --base-url http://localhost:8317 \
  --api-key key1 \
  --management-key 'abc123!!' \
  --model gpt-5.3-codex \
  --samples 5 \
  --raw-scenarios single_turn,multi_turn
```

## 输出产物

结果会写到 `--output-dir/<timestamp>/`：

- `runs.csv`：逐轮记录
- `summary.json`：按组汇总后的结果

`runs.csv` 里每行至少会包含：

- `layer`、`transport`、`scenario`
- `first_event_ms`、`total_ms`
- `input_tokens`、`cached_tokens`、`output_tokens`、`total_tokens`
- `latency_ms_from_logs`
- `ws_cache_hit_rate` 和 prewarm 相关字段

`summary.json` 里会汇总：

- 每组 `p50/p95`
- 总 token
- cache hit rate
- 可选 management usage 交叉校验结果

## 结果怎么看

优先看这几个字段：

- `total_ms_p50`
- `total_ms_p95`
- `usage.total_tokens`
- `usage_cache_hit_rate_percent`

建议的阅读顺序：

1. 先比 `raw/http/multi_turn` 和 `raw/ws/multi_turn`
2. 再看 `cli/http/single_turn` 和 `cli/ws/single_turn`
3. 如果 `raw` 差异小但 `cli` 差异大，优先怀疑 CLI 行为或本机配置
4. 如果 `raw/ws/multi_turn` 的 `cached_tokens / input_tokens` 明显更低，再看 `ws_*` telemetry

经验阈值：

- 延迟差异小于 10%，通常可视为无明显差异
- `total_tokens` 差异小于 5%，通常可视为无明显差异
- `cache_hit_rate` 绝对差超过 10 个百分点，值得继续排查

## 已知限制

- management password 和普通 API key 是两套凭证，不能混用
- `Codex CLI` 黑盒组仍会受本机 `~/.codex/config.toml` 影响
- 不同 auth 可能被轮询到不同上游账号，小样本会有噪音
- management usage 只做组级交叉校验，不做逐轮精确归因
- 如果服务关闭了文件日志或 usage 统计，部分字段会退化为空
- `cli` 组当前是单轮黑盒；多轮 cache 行为的主结论应以 `raw multi_turn` 为准
