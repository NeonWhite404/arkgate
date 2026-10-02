#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""为 internal/catalog/catalog.json 补充缓存单价字段。

为什么需要这个脚本
------------------
内嵌快照当初是从 LiteLLM 目录里「只挑几个字段」裁剪出来的，**没带上缓存价格**
（cache_read_input_token_cost / cache_creation_input_token_cost）。而缓存单价与
输入价差异很大——Anthropic 缓存读约为输入价 10%、写入约 125%——缺了它，计费会把
缓存命中部分按原价多收、把缓存写入漏收。

用法
----
    # 从 LiteLLM 在线目录补齐（需要能访问 GitHub）
    python tools/fill_catalog_cache_prices.py

    # 从本地已下载的目录文件补齐
    python tools/fill_catalog_cache_prices.py --source /path/to/model_prices_and_context_window.json

行为
----
- 就地更新 internal/catalog/catalog.json（先备份为 catalog.json.bak）。
- **只增不改**：仅在条目缺少缓存字段、且上游确实有该字段时才写入；已有值不动。
- 上游查不到的模型保持原样（不写 0，让「未设置」语义与「确认为 0」区分开）。
- 结束时打印统计（新增多少条、跳过多少条），便于人工核对。
"""
import argparse
import json
import pathlib
import sys
import urllib.request

REPO = pathlib.Path(__file__).resolve().parent.parent
SNAPSHOT = REPO / "internal" / "catalog" / "catalog.json"
SOURCE_URL = (
    "https://raw.githubusercontent.com/BerriAI/litellm/main/"
    "model_prices_and_context_window.json"
)

# 只补这两个基础档字段；**不带 _above_XXX_tokens 后缀的分级定价不取**，
# 因为本项目不建模「按上下文长度分档计价」，误用高价档会比不填更糟。
FIELDS = ("cache_read_input_token_cost", "cache_creation_input_token_cost")


def load_source(path):
    if path:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    print(f"从在线目录拉取：{SOURCE_URL}")
    req = urllib.request.Request(SOURCE_URL, headers={"User-Agent": "arkgate-catalog-fill"})
    with urllib.request.urlopen(req, timeout=120) as resp:
        return json.load(resp)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", help="本地 LiteLLM 目录 JSON 路径（省略则在线拉取）")
    ap.add_argument("--dry-run", action="store_true", help="只统计，不写文件")
    args = ap.parse_args()

    try:
        upstream = load_source(args.source)
    except Exception as e:  # noqa: BLE001 - 网络/文件问题都直接报给用户
        print(f"拉取上游目录失败：{e}", file=sys.stderr)
        return 1

    with open(SNAPSHOT, encoding="utf-8") as f:
        snap = json.load(f)

    added, skipped, missing = 0, 0, 0
    for name, entry in snap.items():
        if not isinstance(entry, dict):
            continue
        up = upstream.get(name)
        if not isinstance(up, dict):
            missing += 1
            continue
        changed = False
        for field in FIELDS:
            # 只增不改：快照已有该字段就不动（含显式填的 0）。
            if field in entry:
                continue
            v = up.get(field)
            if isinstance(v, (int, float)) and v > 0:
                entry[field] = v
                changed = True
        if changed:
            added += 1
        else:
            skipped += 1

    print(f"补充缓存价格的条目：{added}")
    print(f"无需改动（已有或上游无值）：{skipped}")
    print(f"上游目录中查不到的模型：{missing}")

    if args.dry_run:
        print("--dry-run：未写入文件")
        return 0

    backup = SNAPSHOT.with_suffix(".json.bak")
    backup.write_bytes(SNAPSHOT.read_bytes())
    with open(SNAPSHOT, "w", encoding="utf-8") as f:
        # 与既有快照保持一致的紧凑格式（ensure_ascii=False 保留中文供应商名）。
        json.dump(snap, f, ensure_ascii=False, separators=(",", ":"))
    print(f"已写入 {SNAPSHOT}（备份：{backup}）")
    print("注意：快照是 go:embed 的，需要重新 go build 才生效。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
