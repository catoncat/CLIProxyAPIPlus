#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "httpx>=0.27.2",
#   "PyYAML>=6.0.2",
#   "websocket-client>=1.8.0",
# ]
# ///

from __future__ import annotations

import argparse
import csv
import json
import math
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib.parse import urljoin, urlparse, urlunparse

import httpx
import yaml
from websocket import WebSocketTimeoutException, create_connection


WS_BETA_HEADER = "responses_websockets=2026-02-06"
DEFAULT_MODEL = "gpt-5.3-codex"
DEFAULT_RAW_SCENARIOS = ("multi_turn",)
DEFAULT_CLI_SCENARIO = "single_turn"
DEFAULT_SAMPLES = 3
DEFAULT_TIMEOUT = 300.0

BACKGROUND_BRIEF = """
Project brief:
CLIProxy is an OpenAI-compatible reverse proxy that can route coding-model traffic to Codex.
The benchmark should focus on transport behavior rather than answer quality.
Keep answers compact and deterministic.
The proxy may expose prompt cache metadata and transport-specific websocket telemetry.
We care about three numbers most: elapsed time, total tokens, and cached tokens.
Avoid long explanations. Prefer short structured output.
""".strip()

TURN_PROMPTS = (
    "Return exactly three bullets summarizing the brief.",
    "Add exactly two concrete risks to the previous answer.",
    'Rewrite the final result as compact JSON with keys "summary" and "risks".',
)

CLI_PROMPT_TEMPLATE = """You are part of a transport benchmark.
Do not run any tools, shell commands, searches, or file reads.
Do not inspect the working directory.

{background}

Task:
{task}

Return compact JSON with exactly these keys:
- "summary": array of 3 short strings
- "risks": array of 2 short strings
"""


@dataclass
class TokenUsage:
    input_tokens: int = 0
    cached_tokens: int = 0
    output_tokens: int = 0
    reasoning_tokens: int = 0
    total_tokens: int = 0

    def add(self, other: "TokenUsage") -> None:
        self.input_tokens += other.input_tokens
        self.cached_tokens += other.cached_tokens
        self.output_tokens += other.output_tokens
        self.reasoning_tokens += other.reasoning_tokens
        self.total_tokens += other.total_tokens

    @property
    def cache_hit_rate(self) -> float:
        if self.input_tokens <= 0:
            return 0.0
        return self.cached_tokens / self.input_tokens

    @classmethod
    def from_response_usage(cls, payload: dict[str, Any] | None) -> "TokenUsage":
        if not payload:
            return cls()
        return cls(
            input_tokens=int(payload.get("input_tokens") or 0),
            cached_tokens=int(((payload.get("input_tokens_details") or {}).get("cached_tokens")) or 0),
            output_tokens=int(payload.get("output_tokens") or 0),
            reasoning_tokens=int(((payload.get("output_tokens_details") or {}).get("reasoning_tokens")) or 0),
            total_tokens=int(payload.get("total_tokens") or 0),
        )

    @classmethod
    def from_cli_usage(cls, payload: dict[str, Any] | None) -> "TokenUsage":
        if not payload:
            return cls()
        input_tokens = int(payload.get("input_tokens") or 0)
        cached_tokens = int(payload.get("cached_input_tokens") or 0)
        output_tokens = int(payload.get("output_tokens") or 0)
        return cls(
            input_tokens=input_tokens,
            cached_tokens=cached_tokens,
            output_tokens=output_tokens,
            reasoning_tokens=0,
            total_tokens=input_tokens + output_tokens,
        )


@dataclass
class WebsocketTelemetry:
    session_id: str = ""
    local_prewarm: int = 0
    upstream_prewarm: int = 0
    prev_response_id_dropped: int = 0
    completed_events: int = 0
    cached_tokens: int = 0
    input_tokens: int = 0
    total_tokens: int = 0
    cache_hit_rate: float = 0.0


@dataclass
class RunResult:
    layer: str
    transport: str
    scenario: str
    iteration: int
    turns: int
    success: bool
    first_event_ms: float | None
    total_ms: float
    latency_ms_from_logs: float | None
    usage: TokenUsage
    response_ids: list[str] = field(default_factory=list)
    websocket_telemetry: WebsocketTelemetry | None = None
    management_usage: TokenUsage | None = None
    output_preview: str = ""
    error: str = ""
    notes: list[str] = field(default_factory=list)

    def to_csv_row(self) -> dict[str, Any]:
        telemetry = self.websocket_telemetry or WebsocketTelemetry()
        mgmt = self.management_usage or TokenUsage()
        return {
            "layer": self.layer,
            "transport": self.transport,
            "scenario": self.scenario,
            "iteration": self.iteration,
            "turns": self.turns,
            "success": self.success,
            "first_event_ms": round_or_none(self.first_event_ms),
            "total_ms": round_or_none(self.total_ms),
            "latency_ms_from_logs": round_or_none(self.latency_ms_from_logs),
            "input_tokens": self.usage.input_tokens,
            "cached_tokens": self.usage.cached_tokens,
            "output_tokens": self.usage.output_tokens,
            "reasoning_tokens": self.usage.reasoning_tokens,
            "total_tokens": self.usage.total_tokens,
            "cache_hit_rate": round(self.usage.cache_hit_rate * 100, 2),
            "mgmt_input_tokens": mgmt.input_tokens,
            "mgmt_cached_tokens": mgmt.cached_tokens,
            "mgmt_output_tokens": mgmt.output_tokens,
            "mgmt_reasoning_tokens": mgmt.reasoning_tokens,
            "mgmt_total_tokens": mgmt.total_tokens,
            "ws_session_id": telemetry.session_id,
            "ws_local_prewarm": telemetry.local_prewarm,
            "ws_upstream_prewarm": telemetry.upstream_prewarm,
            "ws_prev_response_id_dropped": telemetry.prev_response_id_dropped,
            "ws_completed_events": telemetry.completed_events,
            "ws_cached_tokens": telemetry.cached_tokens,
            "ws_input_tokens": telemetry.input_tokens,
            "ws_total_tokens": telemetry.total_tokens,
            "ws_cache_hit_rate": round(telemetry.cache_hit_rate, 2),
            "response_ids": "|".join(self.response_ids),
            "output_preview": self.output_preview,
            "error": self.error,
            "notes": " | ".join(self.notes),
        }


@dataclass
class GroupSummary:
    layer: str
    transport: str
    scenario: str
    runs: int
    success_runs: int
    first_event_ms_p50: float | None
    first_event_ms_p95: float | None
    total_ms_p50: float | None
    total_ms_p95: float | None
    total_ms_mean: float | None
    latency_ms_from_logs_p50: float | None
    usage: TokenUsage
    management_usage: TokenUsage | None
    websocket_sessions: int
    websocket_cache_hit_rate_mean: float | None


class BenchmarkError(RuntimeError):
    pass


def round_or_none(value: float | None) -> float | None:
    if value is None:
        return None
    return round(value, 3)


def percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    ordered = sorted(values)
    if pct <= 0:
        return ordered[0]
    if pct >= 100:
        return ordered[-1]
    index = (len(ordered) - 1) * (pct / 100.0)
    lower = math.floor(index)
    upper = math.ceil(index)
    if lower == upper:
        return ordered[int(index)]
    lower_value = ordered[lower]
    upper_value = ordered[upper]
    weight = index - lower
    return lower_value + (upper_value - lower_value) * weight


def now_local_iso() -> str:
    return datetime.now().astimezone().isoformat(timespec="seconds")


def utc_now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Benchmark Codex HTTP vs websocket v2 through CLIProxy.")
    parser.add_argument("--config-file", default=os.path.expanduser("~/.config/cli-proxy/config.yaml"))
    parser.add_argument("--base-url", help="Proxy base URL, defaults to config.yaml port.")
    parser.add_argument("--api-key", help="Proxy API key for /v1/* requests.")
    parser.add_argument("--management-key", default=os.environ.get("CLIPROXY_MANAGEMENT_KEY"), help="Management password for /v0/management/* cross-checks.")
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--samples", type=int, default=DEFAULT_SAMPLES, help="Runs per group.")
    parser.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT)
    parser.add_argument("--raw-scenarios", default=",".join(DEFAULT_RAW_SCENARIOS), help="Comma-separated raw scenarios: single_turn,multi_turn")
    parser.add_argument("--skip-raw", action="store_true")
    parser.add_argument("--skip-cli", action="store_true")
    parser.add_argument("--codex-bin", default=shutil.which("codex") or "codex")
    parser.add_argument("--cli-provider", default="local")
    parser.add_argument("--cli-workdir", help="Working directory for codex exec. Defaults to a temporary empty directory.")
    parser.add_argument("--output-dir", default="artifacts/codex-transport-bench")
    parser.add_argument("--log-file", help="Main log file path. Defaults to discovered local log file.")
    parser.add_argument("--verbose", action="store_true")
    return parser.parse_args()


def load_yaml(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    with path.open("r", encoding="utf-8") as handle:
        loaded = yaml.safe_load(handle) or {}
    if not isinstance(loaded, dict):
        raise BenchmarkError(f"config is not an object: {path}")
    return loaded


def discover_base_url(config_data: dict[str, Any], explicit: str | None) -> str:
    if explicit:
        return explicit.rstrip("/")
    env_value = os.environ.get("CLIPROXY_API_BASE_URL")
    if env_value:
        return env_value.rstrip("/")
    port = config_data.get("port") or 8317
    return f"http://localhost:{port}"


def split_base_urls(raw_base_url: str) -> tuple[str, str]:
    parsed = urlparse(raw_base_url.rstrip("/"))
    path = parsed.path.rstrip("/")
    if path.endswith("/v1"):
        server_path = path[: -len("/v1")]
        api_path = path
    elif path in {"", "/"}:
        server_path = ""
        api_path = "/v1"
    else:
        server_path = path
        api_path = f"{path}/v1"

    server_base = urlunparse((parsed.scheme, parsed.netloc, server_path, "", "", "")).rstrip("/")
    api_base = urlunparse((parsed.scheme, parsed.netloc, api_path, "", "", "")).rstrip("/")
    return server_base, api_base


def discover_api_key(config_data: dict[str, Any], explicit: str | None) -> str:
    if explicit:
        return explicit
    env_value = os.environ.get("CLIPROXY_API_KEY")
    if env_value:
        return env_value
    keys = config_data.get("api-keys") or []
    if isinstance(keys, list) and keys:
        first = str(keys[0]).strip()
        if first:
            return first
    raise BenchmarkError("missing proxy API key: pass --api-key or set CLIPROXY_API_KEY")


def discover_log_file(config_file: Path, config_data: dict[str, Any], explicit: str | None) -> Path:
    if explicit:
        return Path(explicit).expanduser()

    auth_dir = Path(str(config_data.get("auth-dir") or "~/.config/cli-proxy/auths")).expanduser()
    config_dir = config_file.expanduser().resolve().parent
    candidates = [
        Path(os.path.expanduser("~/.config/cli-proxy/logs/main.log")),
        config_dir / "logs" / "main.log",
        auth_dir / "logs" / "main.log",
        Path.cwd() / "logs" / "main.log",
    ]
    for candidate in candidates:
        if candidate.exists():
            return candidate
    raise BenchmarkError("could not discover main.log; pass --log-file explicitly")


def discover_usage_file(config_file: Path) -> Path:
    config_dir = config_file.expanduser().resolve().parent
    return config_dir / "usage-stats.json"


def management_headers(key: str | None) -> dict[str, str]:
    headers = {"Accept": "application/json"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    return headers


def request_headers(api_key: str) -> dict[str, str]:
    return {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
        "Accept": "application/json",
    }


def build_ws_url(api_base_url: str) -> str:
    parsed = urlparse(api_base_url)
    scheme = parsed.scheme.lower()
    if scheme == "https":
        ws_scheme = "wss"
    else:
        ws_scheme = "ws"
    path = parsed.path.rstrip("/")
    ws_path = f"{path}/responses"
    return urlunparse((ws_scheme, parsed.netloc, ws_path, "", "", ""))


def build_http_responses_url(api_base_url: str) -> str:
    return urljoin(api_base_url.rstrip("/") + "/", "responses")


def parse_duration_to_ms(raw: str) -> float | None:
    value = raw.strip()
    if not value:
        return None
    if value == "0s":
        return 0.0
    total_ms = 0.0
    matched = False
    for amount, unit in re.findall(r"(\d+(?:\.\d+)?)(ms|us|ns|h|m|s)", value):
        matched = True
        number = float(amount)
        if unit == "h":
            total_ms += number * 3600_000
        elif unit == "m":
            total_ms += number * 60_000
        elif unit == "s":
            total_ms += number * 1000
        elif unit == "ms":
            total_ms += number
        elif unit == "us":
            total_ms += number / 1000
        elif unit == "ns":
            total_ms += number / 1_000_000
    if not matched:
        return None
    return total_ms


def parse_gin_latency_ms(lines: list[str], method: str, path: str) -> float | None:
    escaped_method = re.escape(method)
    escaped_path = re.escape(path)
    pattern = re.compile(rf"\] (?P<status>\d+)\s+\|\s+(?P<latency>[^|]+)\|\s+[^|]+\|\s+{escaped_method}\s+\"{escaped_path}\"")
    matches: list[float] = []
    for line in lines:
        match = pattern.search(line)
        if not match:
            continue
        latency = parse_duration_to_ms(match.group("latency"))
        if latency is not None:
            matches.append(latency)
    if not matches:
        return None
    return sum(matches)


def parse_ws_telemetry(lines: list[str]) -> WebsocketTelemetry | None:
    target = None
    for line in lines:
        if "responses websocket telemetry:" in line:
            target = line
    if not target:
        return None
    payload = target.split("responses websocket telemetry:", 1)[1]
    pairs = dict(re.findall(r"([a-z_]+)=([^\s]+)", payload))
    return WebsocketTelemetry(
        session_id=pairs.get("session_id", ""),
        local_prewarm=int(pairs.get("local_prewarm", 0)),
        upstream_prewarm=int(pairs.get("upstream_prewarm", 0)),
        prev_response_id_dropped=int(pairs.get("prev_response_id_dropped", 0)),
        completed_events=int(pairs.get("completed_events", 0)),
        cached_tokens=int(pairs.get("cached_tokens", 0)),
        input_tokens=int(pairs.get("input_tokens", 0)),
        total_tokens=int(pairs.get("total_tokens", 0)),
        cache_hit_rate=float(pairs.get("cache_hit_rate", "0").rstrip("%") or 0.0),
    )


def extract_text_from_output(response_payload: dict[str, Any] | None) -> str:
    if not response_payload:
        return ""
    response = response_payload.get("response") or response_payload
    output = response.get("output") or []
    parts: list[str] = []
    for item in output:
        if not isinstance(item, dict):
            continue
        for content in item.get("content") or []:
            if not isinstance(content, dict):
                continue
            text = content.get("text")
            if isinstance(text, str) and text:
                parts.append(text)
    return "\n".join(parts).strip()


def collapse_preview(text: str, limit: int = 180) -> str:
    compact = " ".join(text.split())
    if len(compact) <= limit:
        return compact
    return compact[: limit - 3] + "..."


class LogProbe:
    def __init__(self, path: Path):
        self.path = path

    def offset(self) -> int:
        if not self.path.exists():
            return 0
        return self.path.stat().st_size

    def read_since(self, offset: int) -> list[str]:
        if not self.path.exists():
            return []
        current_size = self.path.stat().st_size
        if current_size < offset:
            offset = 0
        with self.path.open("r", encoding="utf-8", errors="replace") as handle:
            handle.seek(offset)
            return handle.read().splitlines()


class ManagementClient:
    def __init__(self, server_base_url: str, management_key: str | None, timeout: float):
        self.server_base_url = server_base_url.rstrip("/")
        self.management_key = management_key
        self.timeout = timeout
        self._warned = False

    @property
    def enabled(self) -> bool:
        return bool(self.management_key)

    def get_usage_snapshot(self) -> dict[str, Any] | None:
        if not self.enabled:
            return None
        url = urljoin(self.server_base_url + "/", "v0/management/usage")
        try:
            with httpx.Client(timeout=self.timeout) as client:
                response = client.get(url, headers=management_headers(self.management_key))
                response.raise_for_status()
                return response.json()
        except Exception as exc:  # noqa: BLE001
            if not self._warned:
                print(f"[warn] management usage disabled: {exc}")
                self._warned = True
            return None


def parse_snapshot_timestamp(raw: str | None) -> datetime | None:
    if not raw:
        return None
    try:
        return datetime.fromisoformat(raw)
    except ValueError:
        return None


def usage_from_snapshot_window(snapshot_wrapper: dict[str, Any] | None, start: datetime, end: datetime) -> TokenUsage:
    usage = TokenUsage()
    if not snapshot_wrapper:
        return usage
    root = snapshot_wrapper.get("usage") or {}
    apis = root.get("apis") or {}
    for api_payload in apis.values():
        if not isinstance(api_payload, dict):
            continue
        models = api_payload.get("models") or {}
        for model_payload in models.values():
            if not isinstance(model_payload, dict):
                continue
            details = model_payload.get("details") or []
            for detail in details:
                if not isinstance(detail, dict):
                    continue
                timestamp = parse_snapshot_timestamp(detail.get("timestamp"))
                if timestamp is None:
                    continue
                if timestamp < start or timestamp > end:
                    continue
                tokens = detail.get("tokens") or {}
                usage.add(
                    TokenUsage(
                        input_tokens=int(tokens.get("input_tokens") or 0),
                        cached_tokens=int(tokens.get("cached_tokens") or 0),
                        output_tokens=int(tokens.get("output_tokens") or 0),
                        reasoning_tokens=int(tokens.get("reasoning_tokens") or 0),
                        total_tokens=int(tokens.get("total_tokens") or 0),
                    )
                )
    return usage


class RawResponsesRunner:
    def __init__(self, *, base_url: str, api_key: str, model: str, timeout: float, log_probe: LogProbe):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.timeout = timeout
        self.log_probe = log_probe

    def run(self, transport: str, scenario: str, iteration: int) -> RunResult:
        if scenario not in {"single_turn", "multi_turn"}:
            raise BenchmarkError(f"unsupported raw scenario: {scenario}")
        turns = 1 if scenario == "single_turn" else 3
        request_builder = RawScenarioBuilder(scenario=scenario, model=self.model, iteration=iteration)

        if transport == "http":
            return self._run_http(iteration, turns, request_builder)
        if transport == "ws":
            return self._run_ws(iteration, turns, request_builder)
        raise BenchmarkError(f"unsupported raw transport: {transport}")

    def _run_http(self, iteration: int, turns: int, request_builder: "RawScenarioBuilder") -> RunResult:
        url = build_http_responses_url(self.base_url)
        headers = request_headers(self.api_key)
        usage = TokenUsage()
        response_ids: list[str] = []
        output_text = ""
        turn_outputs: list[str] = []
        first_event_ms: float | None = None
        total_start = time.perf_counter()
        log_offset = self.log_probe.offset()
        error = ""

        try:
            with httpx.Client(timeout=self.timeout) as client:
                previous_response_id = ""
                for turn_index in range(turns):
                    payload = request_builder.http_payload(turn_index, previous_response_id, turn_outputs)
                    turn_result = execute_http_stream(client, url, headers, payload)
                    if first_event_ms is None:
                        first_event_ms = turn_result["first_event_ms"]
                    previous_response_id = turn_result["response_id"]
                    if previous_response_id:
                        response_ids.append(previous_response_id)
                    turn_outputs.append(turn_result["output_text"])
                    output_text = turn_result["output_text"] or output_text
                    usage.add(turn_result["usage"])
        except Exception as exc:  # noqa: BLE001
            error = str(exc)

        total_ms = (time.perf_counter() - total_start) * 1000
        time.sleep(0.35)
        lines = self.log_probe.read_since(log_offset)
        latency_ms = parse_gin_latency_ms(lines, "POST", "/v1/responses")
        success = error == ""
        return RunResult(
            layer="raw",
            transport="http",
            scenario=request_builder.scenario,
            iteration=iteration,
            turns=turns,
            success=success,
            first_event_ms=first_event_ms,
            total_ms=total_ms,
            latency_ms_from_logs=latency_ms,
            usage=usage,
            response_ids=response_ids,
            output_preview=collapse_preview(output_text),
            error=error,
        )

    def _run_ws(self, iteration: int, turns: int, request_builder: "RawScenarioBuilder") -> RunResult:
        ws_url = build_ws_url(self.base_url)
        headers = [
            f"Authorization: Bearer {self.api_key}",
            f"OpenAI-Beta: {WS_BETA_HEADER}",
        ]
        usage = TokenUsage()
        response_ids: list[str] = []
        output_text = ""
        turn_outputs: list[str] = []
        first_event_ms: float | None = None
        total_start = time.perf_counter()
        log_offset = self.log_probe.offset()
        error = ""

        websocket_conn = None
        try:
            websocket_conn = create_connection(ws_url, header=headers, timeout=self.timeout)
            previous_response_id = ""
            for turn_index in range(turns):
                payload = request_builder.ws_payload(turn_index, previous_response_id, turn_outputs)
                turn_result = execute_websocket_turn(websocket_conn, payload)
                if first_event_ms is None:
                    first_event_ms = turn_result["first_event_ms"]
                previous_response_id = turn_result["response_id"]
                if previous_response_id:
                    response_ids.append(previous_response_id)
                turn_outputs.append(turn_result["output_text"])
                output_text = turn_result["output_text"] or output_text
                usage.add(turn_result["usage"])
        except Exception as exc:  # noqa: BLE001
            error = str(exc)
        finally:
            if websocket_conn is not None:
                try:
                    websocket_conn.close()
                except Exception:  # noqa: BLE001
                    pass

        total_ms = (time.perf_counter() - total_start) * 1000
        time.sleep(0.45)
        lines = self.log_probe.read_since(log_offset)
        latency_ms = parse_gin_latency_ms(lines, "GET", "/v1/responses")
        telemetry = parse_ws_telemetry(lines)
        success = error == ""
        return RunResult(
            layer="raw",
            transport="ws",
            scenario=request_builder.scenario,
            iteration=iteration,
            turns=turns,
            success=success,
            first_event_ms=first_event_ms,
            total_ms=total_ms,
            latency_ms_from_logs=latency_ms,
            usage=usage,
            response_ids=response_ids,
            websocket_telemetry=telemetry,
            output_preview=collapse_preview(output_text),
            error=error,
        )


class RawScenarioBuilder:
    def __init__(self, *, scenario: str, model: str, iteration: int):
        self.scenario = scenario
        self.model = model
        self.iteration = iteration
        self.benchmark_id = f"bench-{scenario}-{iteration:03d}-{int(time.time())}"
        self.instructions = BACKGROUND_BRIEF

    def http_payload(self, turn_index: int, previous_response_id: str, turn_outputs: list[str]) -> dict[str, Any]:
        if self.scenario == "single_turn":
            return {
                "model": self.model,
                "stream": True,
                "instructions": self.instructions,
                "input": [build_message(self._single_turn_prompt())],
            }

        prompt = self._http_multi_turn_prompt(turn_index, turn_outputs)
        payload = {
            "model": self.model,
            "stream": True,
            "instructions": self.instructions,
            "input": [build_message(prompt)],
        }
        if previous_response_id:
            payload["previous_response_id"] = previous_response_id
        return payload

    def ws_payload(self, turn_index: int, previous_response_id: str, turn_outputs: list[str]) -> dict[str, Any]:
        if self.scenario == "single_turn":
            return {
                "type": "response.create",
                "model": self.model,
                "instructions": self.instructions,
                "input": [build_message(self._single_turn_prompt())],
            }

        prompt = self._ws_multi_turn_prompt(turn_index)
        payload = {
            "type": "response.create",
            "model": self.model,
            "instructions": self.instructions,
            "input": [build_message(prompt)],
        }
        if previous_response_id:
            payload["previous_response_id"] = previous_response_id
        return payload

    def _single_turn_prompt(self) -> str:
        return f"Task:\n{TURN_PROMPTS[0]}\nOutput JSON with keys \"summary\" and \"risks\". Use an empty array for \"risks\"."

    def _http_multi_turn_prompt(self, turn_index: int, turn_outputs: list[str]) -> str:
        if turn_index == 0:
            return f"Task:\n{TURN_PROMPTS[0]}"
        prior_output = turn_outputs[-1] if turn_outputs else ""
        return (
            f"Previous assistant output:\n{prior_output}\n\n"
            f"Task:\n{TURN_PROMPTS[turn_index]}"
        )

    def _ws_multi_turn_prompt(self, turn_index: int) -> str:
        if turn_index == 0:
            return f"{BACKGROUND_BRIEF}\n\nTask:\n{TURN_PROMPTS[0]}"
        return f"Task:\n{TURN_PROMPTS[turn_index]}"


def build_message(text: str) -> dict[str, Any]:
    return {
        "role": "user",
        "content": [{"type": "input_text", "text": text}],
    }


def execute_http_stream(client: httpx.Client, url: str, headers: dict[str, str], payload: dict[str, Any]) -> dict[str, Any]:
    first_event_ms = None
    start = time.perf_counter()
    response_id = ""
    completed_payload: dict[str, Any] | None = None

    with client.stream("POST", url, headers=headers, json=payload) as response:
        response.raise_for_status()
        for line in response.iter_lines():
            if not line:
                continue
            if isinstance(line, bytes):
                line = line.decode("utf-8", errors="replace")
            if not line.startswith("data: "):
                continue
            data = line[6:]
            if data == "[DONE]":
                break
            if first_event_ms is None:
                first_event_ms = (time.perf_counter() - start) * 1000
            event = json.loads(data)
            event_type = event.get("type")
            if event_type == "response.created":
                response_id = ((event.get("response") or {}).get("id")) or response_id
            if event_type == "response.completed":
                completed_payload = event
                response_id = ((event.get("response") or {}).get("id")) or response_id

    if completed_payload is None:
        raise BenchmarkError("HTTP stream ended without response.completed")
    response_obj = completed_payload.get("response") or {}
    return {
        "first_event_ms": first_event_ms,
        "response_id": response_id,
        "usage": TokenUsage.from_response_usage(response_obj.get("usage")),
        "output_text": extract_text_from_output(response_obj),
    }


def execute_websocket_turn(websocket_conn: Any, payload: dict[str, Any]) -> dict[str, Any]:
    start = time.perf_counter()
    first_event_ms = None
    response_id = ""
    completed_payload: dict[str, Any] | None = None
    websocket_conn.send(json.dumps(payload))

    while True:
        try:
            raw = websocket_conn.recv()
        except WebSocketTimeoutException as exc:
            raise BenchmarkError("websocket timed out waiting for response") from exc
        if first_event_ms is None:
            first_event_ms = (time.perf_counter() - start) * 1000
        if not raw:
            continue
        event = json.loads(raw)
        event_type = event.get("type")
        if event_type == "response.created":
            response_id = ((event.get("response") or {}).get("id")) or response_id
            continue
        if event_type in {"response.completed", "response.done"}:
            completed_payload = event
            response_id = ((event.get("response") or {}).get("id")) or response_id
            break
        if event_type == "error":
            raise BenchmarkError(json.dumps(event, ensure_ascii=False))

    response_obj = completed_payload.get("response") or {}
    return {
        "first_event_ms": first_event_ms,
        "response_id": response_id,
        "usage": TokenUsage.from_response_usage(response_obj.get("usage")),
        "output_text": extract_text_from_output(response_obj),
    }


class CodexCLIRunner:
    def __init__(self, *, codex_bin: str, base_url: str, model: str, provider: str, timeout: float, log_probe: LogProbe, workdir: Path):
        self.codex_bin = codex_bin
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.provider = provider
        self.timeout = timeout
        self.log_probe = log_probe
        self.workdir = workdir

    def run(self, transport: str, iteration: int) -> RunResult:
        supports_websockets = transport == "ws"
        prompt = CLI_PROMPT_TEMPLATE.format(background=BACKGROUND_BRIEF, task=TURN_PROMPTS[0])
        command = [
            self.codex_bin,
            "exec",
            "--json",
            "--ephemeral",
            "--skip-git-repo-check",
            "-C",
            str(self.workdir),
            "-c",
            f'model_provider="{self.provider}"',
            "-c",
            f'model="{self.model}"',
            "-c",
            f'model_providers.{self.provider}.base_url="{self.base_url}"',
            "-c",
            f'model_providers.{self.provider}.wire_api="responses"',
            "-c",
            f"model_providers.{self.provider}.supports_websockets={'true' if supports_websockets else 'false'}",
            "-c",
            'features.responses_websockets_v2=true',
            "-c",
            'model_reasoning_effort="none"',
            prompt,
        ]

        start = time.perf_counter()
        first_event_ms = None
        usage = TokenUsage()
        output_text = ""
        error = ""
        log_offset = self.log_probe.offset()

        try:
            process = subprocess.Popen(
                command,
                cwd=self.workdir,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
                encoding="utf-8",
                errors="replace",
            )
            stdout_lines: list[str] = []
            stderr_lines: list[str] = []
            assert process.stdout is not None
            while True:
                line = process.stdout.readline()
                if line == "" and process.poll() is not None:
                    break
                if not line:
                    continue
                stdout_lines.append(line.rstrip("\n"))
                event = json.loads(line)
                if event.get("type") == "turn.started":
                    continue
                if first_event_ms is None:
                    first_event_ms = (time.perf_counter() - start) * 1000
                if event.get("type") == "item.completed":
                    item = event.get("item") or {}
                    if item.get("type") == "agent_message":
                        output_text = item.get("text") or output_text
                if event.get("type") == "turn.completed":
                    usage = TokenUsage.from_cli_usage(event.get("usage"))
            assert process.stderr is not None
            stderr_lines.extend(line.rstrip("\n") for line in process.stderr)
            return_code = process.wait(timeout=5)
            if return_code != 0:
                error = "\n".join(stderr_lines[-10:] or stdout_lines[-10:]) or f"codex exited with {return_code}"
        except subprocess.TimeoutExpired as exc:
            error = f"codex wait timeout: {exc}"
        except Exception as exc:  # noqa: BLE001
            error = str(exc)

        total_ms = (time.perf_counter() - start) * 1000
        time.sleep(0.45)
        lines = self.log_probe.read_since(log_offset)
        method = "GET" if transport == "ws" else "POST"
        latency_ms = parse_gin_latency_ms(lines, method, "/v1/responses")
        telemetry = parse_ws_telemetry(lines) if transport == "ws" else None
        return RunResult(
            layer="cli",
            transport=transport,
            scenario=DEFAULT_CLI_SCENARIO,
            iteration=iteration,
            turns=1,
            success=error == "",
            first_event_ms=first_event_ms,
            total_ms=total_ms,
            latency_ms_from_logs=latency_ms,
            usage=usage,
            websocket_telemetry=telemetry,
            output_preview=collapse_preview(output_text),
            error=error,
        )


def summarise_group(results: list[RunResult]) -> GroupSummary:
    if not results:
        raise BenchmarkError("cannot summarise empty group")
    usage = TokenUsage()
    mgmt_usage = TokenUsage()
    mgmt_seen = False
    ws_rates: list[float] = []
    first_events = [result.first_event_ms for result in results if result.first_event_ms is not None]
    totals = [result.total_ms for result in results]
    latencies = [result.latency_ms_from_logs for result in results if result.latency_ms_from_logs is not None]

    for result in results:
        usage.add(result.usage)
        if result.management_usage is not None:
            mgmt_seen = True
            mgmt_usage.add(result.management_usage)
        if result.websocket_telemetry is not None:
            ws_rates.append(result.websocket_telemetry.cache_hit_rate)

    return GroupSummary(
        layer=results[0].layer,
        transport=results[0].transport,
        scenario=results[0].scenario,
        runs=len(results),
        success_runs=sum(1 for result in results if result.success),
        first_event_ms_p50=percentile([value for value in first_events if value is not None], 50),
        first_event_ms_p95=percentile([value for value in first_events if value is not None], 95),
        total_ms_p50=percentile(totals, 50),
        total_ms_p95=percentile(totals, 95),
        total_ms_mean=statistics.mean(totals) if totals else None,
        latency_ms_from_logs_p50=percentile([value for value in latencies if value is not None], 50),
        usage=usage,
        management_usage=mgmt_usage if mgmt_seen else None,
        websocket_sessions=sum(1 for result in results if result.websocket_telemetry is not None),
        websocket_cache_hit_rate_mean=(statistics.mean(ws_rates) if ws_rates else None),
    )


def ensure_output_dir(root: Path) -> Path:
    timestamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    outdir = root / timestamp
    outdir.mkdir(parents=True, exist_ok=False)
    return outdir


def write_runs_csv(outdir: Path, results: list[RunResult]) -> Path:
    path = outdir / "runs.csv"
    rows = [result.to_csv_row() for result in results]
    fieldnames = list(rows[0].keys()) if rows else []
    with path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(rows)
    return path


def write_summary_json(outdir: Path, payload: dict[str, Any]) -> Path:
    path = outdir / "summary.json"
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2)
    return path


def attach_management_usage(results: list[RunResult], group_usage: TokenUsage | None) -> None:
    if group_usage is None:
        return
    if not results:
        return
    total_tokens = sum(result.usage.total_tokens for result in results if result.success)
    if total_tokens <= 0:
        print("[warn] skip management usage attachment: no successful direct token totals")
        return
    ratio = group_usage.total_tokens / total_tokens if total_tokens else 0.0
    if ratio < 0.5 or ratio > 2.0:
        print(
            "[warn] skip management usage attachment: "
            f"group_total_tokens={group_usage.total_tokens} direct_total_tokens={total_tokens}"
        )
        return
    for result in results:
        ratio = result.usage.total_tokens / total_tokens if total_tokens else 0.0
        result.management_usage = TokenUsage(
            input_tokens=int(round(group_usage.input_tokens * ratio)),
            cached_tokens=int(round(group_usage.cached_tokens * ratio)),
            output_tokens=int(round(group_usage.output_tokens * ratio)),
            reasoning_tokens=int(round(group_usage.reasoning_tokens * ratio)),
            total_tokens=int(round(group_usage.total_tokens * ratio)),
        )


def print_group_summary(summary: GroupSummary) -> None:
    cache_rate = round(summary.usage.cache_hit_rate * 100, 2)
    total_p50 = round_or_none(summary.total_ms_p50)
    total_p95 = round_or_none(summary.total_ms_p95)
    first_p50 = round_or_none(summary.first_event_ms_p50)
    print(
        f"[group] {summary.layer}/{summary.transport}/{summary.scenario} "
        f"runs={summary.runs} success={summary.success_runs} "
        f"first_p50_ms={first_p50} total_p50_ms={total_p50} total_p95_ms={total_p95} "
        f"tokens={summary.usage.total_tokens} cache_hit={cache_rate}%"
    )


def run_group(
    *,
    group_name: tuple[str, str, str],
    runner: Any,
    samples: int,
    management_client: ManagementClient,
) -> tuple[list[RunResult], GroupSummary]:
    layer, transport, scenario = group_name
    print(f"[start] {layer}/{transport}/{scenario} samples={samples}")
    group_start = datetime.now().astimezone()
    results: list[RunResult] = []

    for iteration in range(1, samples + 1):
        print(f"  - run {iteration}/{samples}")
        result = runner.run(transport, scenario, iteration) if layer == "raw" else runner.run(transport, iteration)
        results.append(result)
        status = "ok" if result.success else "error"
        cache_pct = round(result.usage.cache_hit_rate * 100, 2)
        print(
            f"    {status}: total_ms={round(result.total_ms, 1)} "
            f"input={result.usage.input_tokens} cached={result.usage.cached_tokens} "
            f"total_tokens={result.usage.total_tokens} cache_hit={cache_pct}%"
        )

    group_end = datetime.now().astimezone()
    after_snapshot = management_client.get_usage_snapshot() if management_client.enabled else None
    management_usage = usage_from_snapshot_window(after_snapshot, group_start, group_end) if after_snapshot else None
    attach_management_usage(results, management_usage)
    summary = summarise_group(results)
    print_group_summary(summary)
    return results, summary


def build_summary_payload(
    *,
    args: argparse.Namespace,
    base_url: str,
    log_file: Path,
    usage_file: Path,
    runs_csv: Path,
    results: list[RunResult],
    group_summaries: list[GroupSummary],
) -> dict[str, Any]:
    return {
        "generated_at": utc_now_iso(),
        "config": {
            "base_url": base_url,
            "model": args.model,
            "samples": args.samples,
            "raw_scenarios": [scenario for scenario in args.raw_scenarios.split(",") if scenario],
            "skip_raw": args.skip_raw,
            "skip_cli": args.skip_cli,
            "log_file": str(log_file),
            "usage_file": str(usage_file),
            "codex_bin": args.codex_bin,
            "cli_provider": args.cli_provider,
        },
        "artifacts": {
            "runs_csv": str(runs_csv),
        },
        "groups": [
            {
                "layer": summary.layer,
                "transport": summary.transport,
                "scenario": summary.scenario,
                "runs": summary.runs,
                "success_runs": summary.success_runs,
                "first_event_ms_p50": round_or_none(summary.first_event_ms_p50),
                "first_event_ms_p95": round_or_none(summary.first_event_ms_p95),
                "total_ms_p50": round_or_none(summary.total_ms_p50),
                "total_ms_p95": round_or_none(summary.total_ms_p95),
                "total_ms_mean": round_or_none(summary.total_ms_mean),
                "latency_ms_from_logs_p50": round_or_none(summary.latency_ms_from_logs_p50),
                "usage": asdict(summary.usage),
                "usage_cache_hit_rate_percent": round(summary.usage.cache_hit_rate * 100, 2),
                "management_usage": asdict(summary.management_usage) if summary.management_usage else None,
                "management_cache_hit_rate_percent": (
                    round(summary.management_usage.cache_hit_rate * 100, 2) if summary.management_usage else None
                ),
                "websocket_sessions": summary.websocket_sessions,
                "websocket_cache_hit_rate_mean_percent": round_or_none(summary.websocket_cache_hit_rate_mean),
            }
            for summary in group_summaries
        ],
        "runs": [result.to_csv_row() for result in results],
    }


def main() -> int:
    args = parse_args()
    config_file = Path(args.config_file).expanduser()
    config_data = load_yaml(config_file)
    discovered_base_url = discover_base_url(config_data, args.base_url)
    server_base_url, api_base_url = split_base_urls(discovered_base_url)
    api_key = discover_api_key(config_data, args.api_key)
    log_file = discover_log_file(config_file, config_data, args.log_file)
    usage_file = discover_usage_file(config_file)

    if args.verbose:
        print(f"[info] started_at={now_local_iso()}")
        print(f"[info] server_base_url={server_base_url}")
        print(f"[info] api_base_url={api_base_url}")
        print(f"[info] config_file={config_file}")
        print(f"[info] log_file={log_file}")
        print(f"[info] usage_file={usage_file}")

    outdir = ensure_output_dir(Path(args.output_dir))
    log_probe = LogProbe(log_file)
    management_client = ManagementClient(server_base_url=server_base_url, management_key=args.management_key, timeout=args.timeout)

    results: list[RunResult] = []
    group_summaries: list[GroupSummary] = []

    cli_workdir: Path
    temp_dir: tempfile.TemporaryDirectory[str] | None = None
    if args.cli_workdir:
        cli_workdir = Path(args.cli_workdir).expanduser().resolve()
        cli_workdir.mkdir(parents=True, exist_ok=True)
    else:
        temp_dir = tempfile.TemporaryDirectory(prefix="codex-transport-bench-")
        cli_workdir = Path(temp_dir.name)

    try:
        if not args.skip_raw:
            raw_runner = RawResponsesRunner(
                base_url=api_base_url,
                api_key=api_key,
                model=args.model,
                timeout=args.timeout,
                log_probe=log_probe,
            )
            raw_scenarios = [item.strip() for item in args.raw_scenarios.split(",") if item.strip()]
            for scenario in raw_scenarios:
                for transport in ("http", "ws"):
                    group_results, summary = run_group(
                        group_name=("raw", transport, scenario),
                        runner=raw_runner,
                        samples=args.samples,
                        management_client=management_client,
                    )
                    results.extend(group_results)
                    group_summaries.append(summary)

        if not args.skip_cli:
            cli_runner = CodexCLIRunner(
                codex_bin=args.codex_bin,
                base_url=api_base_url,
                model=args.model,
                provider=args.cli_provider,
                timeout=args.timeout,
                log_probe=log_probe,
                workdir=cli_workdir,
            )
            for transport in ("http", "ws"):
                group_results, summary = run_group(
                    group_name=("cli", transport, DEFAULT_CLI_SCENARIO),
                    runner=cli_runner,
                    samples=args.samples,
                    management_client=management_client,
                )
                results.extend(group_results)
                group_summaries.append(summary)
    finally:
        if temp_dir is not None:
            temp_dir.cleanup()

    if not results:
        raise BenchmarkError("nothing ran; remove --skip-raw/--skip-cli or add raw scenarios")

    runs_csv = write_runs_csv(outdir, results)
    summary_json = write_summary_json(
        outdir,
        build_summary_payload(
            args=args,
            base_url=api_base_url,
            log_file=log_file,
            usage_file=usage_file,
            runs_csv=runs_csv,
            results=results,
            group_summaries=group_summaries,
        ),
    )
    print(f"[done] runs_csv={runs_csv}")
    print(f"[done] summary_json={summary_json}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        raise SystemExit(130)
    except BenchmarkError as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(2)
