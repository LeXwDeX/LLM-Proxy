#!/usr/bin/env python3
"""
从 models.dev/api.json 生成项目内部模型目录。

数据源的价格单位是 $/百万 tokens。本脚本保持 $/百万 tokens 存储。

用法:
  curl -sS https://models.dev/api.json -o /tmp/models_dev_raw.json
  python3 scripts/update-model-catalog.py [input.json] [output.json]

默认:
  input  = /tmp/models_dev_raw.json
  output = internal/catalog/data/models.json
"""

import json
import sys
from typing import Any, Optional


# models.dev provider id -> 本项目 endpoint_type 的兼容映射。
# 未列出的 provider 不再丢弃，直接使用 models.dev 的 provider id 作为 endpoint_type。
PROVIDER_MAP = {
    "openai": "openai",
    "azure": "azure_openai",
    "anthropic": "claude",
    "google": "gemini",
    "github-copilot": "copilot",
    "alibaba": "bailian",
}


def endpoint_type_for_provider(provider_id: str) -> str:
    """返回 provider 对应的 endpoint_type；未知 provider 保持原始 id。"""
    return PROVIDER_MAP.get(provider_id, provider_id)


def convert_cost(cost_obj: dict, provider_id: str = "", model_id: str = "") -> Optional[dict]:
    """将 models.dev 的 cost ($/M tokens) 转为项目的 $/M tokens。

    Anthropic (claude) 兜底：当 cache_write 价缺失或为 0 且 input 价 > 0 时，
    按 Anthropic 官方定价规则 cache_write = input × 1.25 补齐。
    """
    if not cost_obj:
        return None

    result: dict[str, float] = {}

    input_price = cost_obj.get("input")
    output_price = cost_obj.get("output")
    cache_read_price = cost_obj.get("cache_read")
    cache_write_price = cost_obj.get("cache_write", 0)

    if isinstance(input_price, (int, float)) and input_price > 0:
        result["input_per_1m_tokens"] = round(input_price, 10)
    if isinstance(output_price, (int, float)) and output_price > 0:
        result["output_per_1m_tokens"] = round(output_price, 10)
    if isinstance(cache_write_price, (int, float)) and cache_write_price > 0:
        result["cache_write_per_1m_tokens"] = round(cache_write_price, 10)
    if isinstance(cache_read_price, (int, float)) and cache_read_price > 0:
        result["cache_read_per_1m_tokens"] = round(cache_read_price, 10)

    # Anthropic 兜底：cache_write = input × 1.25（官方 prompt caching 定价）
    is_anthropic = provider_id == "anthropic" or "claude" in model_id.lower()
    if is_anthropic and "cache_write_per_1m_tokens" not in result:
        if isinstance(input_price, (int, float)) and input_price > 0:
            result["cache_write_per_1m_tokens"] = round(input_price * 1.25, 10)

    return result if result else None


def extract_capabilities(m: dict) -> list[str]:
    """从模型元数据中提取 capabilities 标签。"""
    caps = []
    modalities = m.get("modalities", {})
    input_mods = modalities.get("input", [])
    output_mods = modalities.get("output", [])

    if "image" in input_mods:
        caps.append("vision")
    if "image" in output_mods:
        caps.append("image_generation")
    if "audio" in input_mods:
        caps.append("audio_input")
    if "audio" in output_mods:
        caps.append("audio_output")
    if m.get("tool_call"):
        caps.append("function_calling")
    if m.get("structured_output"):
        caps.append("structured_output")
    if m.get("reasoning"):
        caps.append("reasoning")
    return caps


def transform_model(provider_id: str, model_id: str, m: dict) -> Optional[dict]:
    """将外部模型数据转换为项目内部格式。"""
    endpoint_type = endpoint_type_for_provider(provider_id)

    entry: dict[str, Any] = {
        "endpoint_type": endpoint_type,
        "model": model_id.lower().strip(),
    }

    # display_name
    name = m.get("name", "")
    if name and name.lower() != model_id.lower():
        entry["display_name"] = name

    # 费用
    cost = convert_cost(m.get("cost", {}), provider_id, model_id)
    if cost:
        entry["default_cost"] = cost

    # capabilities
    caps = extract_capabilities(m)
    if caps:
        entry["capabilities"] = caps

    # limit 字段
    limit = m.get("limit", {})
    ctx = limit.get("context")
    if isinstance(ctx, (int, float)) and ctx > 0:
        entry["context_window"] = int(ctx)
    out = limit.get("output")
    if isinstance(out, (int, float)) and out > 0:
        entry["max_output_tokens"] = int(out)
    inp = limit.get("input")
    if isinstance(inp, (int, float)) and inp > 0:
        entry["max_input_tokens"] = int(inp)

    # family 字段
    family = m.get("family", "")
    if family:
        entry["model_family"] = family

    return entry


def _supplementary_models() -> list[dict]:
    """models.dev 中缺失但项目需要的模型条目（估算价格，仅供参考）。"""
    extras: list[dict] = []

    # --- OpenAI: 图像、音频类模型 ---
    image_models = [
        (
            "dall-e-3",
            "DALL·E 3",
            {"input_per_1m_tokens": 40, "output_per_1m_tokens": 80},
        ),
        (
            "dall-e-2",
            "DALL·E 2",
            {"input_per_1m_tokens": 20, "output_per_1m_tokens": 20},
        ),
        (
            "gpt-image-1",
            "GPT Image 1",
            {"input_per_1m_tokens": 5, "output_per_1m_tokens": 40},
        ),
        (
            "gpt-image-1.5",
            "GPT Image 1.5",
            {"input_per_1m_tokens": 5, "output_per_1m_tokens": 40},
        ),
    ]
    for model_id, name, cost in image_models:
        extras.append(
            {
                "endpoint_type": "openai",
                "model": model_id,
                "display_name": name,
                "default_cost": cost,
                "capabilities": ["image_generation"],
            }
        )

    audio_models = [
        ("tts-1", "TTS 1", {"input_per_1m_tokens": 15}),
        ("tts-1-hd", "TTS 1 HD", {"input_per_1m_tokens": 30}),
        ("whisper-1", "Whisper 1", {"input_per_1m_tokens": 6}),
    ]
    for model_id, name, cost in audio_models:
        extras.append(
            {
                "endpoint_type": "openai",
                "model": model_id,
                "display_name": name,
                "default_cost": cost,
                "capabilities": ["audio_input"]
                if "whisper" in model_id
                else ["audio_output"],
            }
        )

    # --- OpenAI: gpt-5.2-chat (azure 有 但 openai provider 缺) ---
    extras.append(
        {
            "endpoint_type": "openai",
            "model": "gpt-5.2-chat",
            "display_name": "GPT-5.2 Chat",
            "default_cost": {
                "input_per_1m_tokens": 2,
                "output_per_1m_tokens": 8,
                "cache_write_per_1m_tokens": 0.2,
            },
            "capabilities": ["vision", "function_calling", "structured_output"],
        }
    )

    # GPT-5.5 fallback: OpenAI standard short-context USD / 1M (2026-09-09).
    # https://developers.openai.com/api/docs/models/gpt-5.5
    # Azure retains reference pricing here; deployment-specific prices use templates.
    gpt55_cost = {
        "input_per_1m_tokens": 5,
        "output_per_1m_tokens": 30,
        "cache_read_per_1m_tokens": 0.5,
    }
    gpt55_caps = ["vision", "function_calling", "structured_output", "reasoning"]
    for ep in ("azure_openai", "openai"):
        extras.append(
            {
                "endpoint_type": ep,
                "model": "gpt-5.5",
                "display_name": "GPT-5.5",
                "default_cost": gpt55_cost,
                "capabilities": list(gpt55_caps),
                "model_family": "gpt-5",
            }
        )
    # OpenAI 还有 gpt-5.5-pro 变体
    extras.append(
        {
            "endpoint_type": "openai",
            "model": "gpt-5.5-pro",
            "display_name": "GPT-5.5 Pro",
            "default_cost": {"input_per_1m_tokens": 30, "output_per_1m_tokens": 180},
            "capabilities": list(gpt55_caps),
            "model_family": "gpt-5",
        }
    )

    # --- GPT-IMAGE-2 (Preview): models.dev 暂未收录 ---
    # 网宿图像通道在下方 wangsu_image_models 已有；此处补 azure_openai / openai
    gpt_image2_cost = {"input_per_1m_tokens": 5, "output_per_1m_tokens": 40}
    for ep in ("azure_openai", "openai"):
        extras.append(
            {
                "endpoint_type": ep,
                "model": "gpt-image-2",
                "display_name": "GPT Image 2 (Preview)",
                "aliases": ["gpt-image-2-2026-04-21"],
                "default_cost": gpt_image2_cost,
                "capabilities": ["image_generation"],
                "model_family": "gpt-image",
            }
        )

    # --- Azure OpenAI: 同样补充图像和音频模型 ---
    for model_id, name, cost in image_models:
        extras.append(
            {
                "endpoint_type": "azure_openai",
                "model": model_id,
                "display_name": name,
                "default_cost": cost,
                "capabilities": ["image_generation"],
            }
        )
    for model_id, name, cost in audio_models:
        extras.append(
            {
                "endpoint_type": "azure_openai",
                "model": model_id,
                "display_name": name,
                "default_cost": cost,
                "capabilities": ["audio_input"]
                if "whisper" in model_id
                else ["audio_output"],
            }
        )

    # --- Claude: 仅保留 dot-notation 变体作为独立条目 ---
    # 注意: claude-sonnet-4, claude-opus-4, claude-4-sonnet, claude-4-opus, claude-sonnet-4.5
    #       以及 claude-3.5-* 变体均通过 _add_aliases() 添加为别名，不再创建独立条目。

    # --- 网宿图像通道（独立终态 URL）：文生图 + 图编辑 ---
    # endpoint_type 不在 wangsuToCanonical 映射中，需独立注册。
    # gpt-image-2 的元数据来自 OpenAI 官方文档（models.dev 暂未收录）。
    wangsu_image_models = [
        ("gpt-image-1.5", "GPT Image 1.5", {"input_per_1m_tokens": 5, "output_per_1m_tokens": 40}, []),
        (
            "gpt-image-2",
            "GPT Image 2",
            {"input_per_1m_tokens": 5, "output_per_1m_tokens": 40},
            ["gpt-image-2-2026-04-21"],
        ),
    ]
    for model_id, name, cost, aliases in wangsu_image_models:
        # 文生图通道
        gen_entry = {
            "endpoint_type": "wangsu_openai_image",
            "model": model_id,
            "display_name": f"{name} (Wangsu - Generations)",
            "default_cost": cost,
            "capabilities": ["image_generation"],
            "model_family": "gpt-image",
        }
        if aliases:
            gen_entry["aliases"] = list(aliases)
        extras.append(gen_entry)
        # 图编辑通道（含 vision 能力）
        edit_entry = {
            "endpoint_type": "wangsu_openai_image_edit",
            "model": model_id,
            "display_name": f"{name} (Wangsu - Edits)",
            "default_cost": cost,
            "capabilities": ["vision", "image_generation"],
            "model_family": "gpt-image",
        }
        if aliases:
            edit_entry["aliases"] = list(aliases)
        extras.append(edit_entry)

    # --- DeepSeek（models.dev 无收录，价格按官方 CNY 汇率 7.2 换算 USD/1M tokens）---
    # V4 Flash: 输入 1 CNY/1M → 0.139 USD；输出 2 CNY/1M → 0.278 USD；缓存命中 0.02 → 0.0028
    # V4 Pro:   输入 12 CNY/1M → 1.667 USD；输出 24 CNY/1M → 3.333 USD；缓存命中 0.1 → 0.0139
    # （V4 Pro 使用原价；2.5 折优惠至 2026-05-31，届时可手动修正）
    # deepseek-chat / deepseek-reasoner 为弃用兼容别名（→ 2026-07-24），复用对应正式名的价格
    deepseek_models = [
        {
            "endpoint_type": "deepseek",
            "model": "deepseek-v4-flash",
            "display_name": "DeepSeek V4 Flash",
            "aliases": ["deepseek-chat"],
            "capabilities": ["chat", "function_calling", "structured_output"],
            "model_family": "deepseek-v4",
            "context_window": 1000000,
            "max_output_tokens": 32768,
            "default_cost": {
                "input_per_1m_tokens": 0.139,
                "output_per_1m_tokens": 0.278,
                "cache_write_per_1m_tokens": 0.0028,
            },
        },
        {
            "endpoint_type": "deepseek",
            "model": "deepseek-v4-pro",
            "display_name": "DeepSeek V4 Pro",
            "aliases": ["deepseek-reasoner"],
            "capabilities": ["chat", "reasoning", "function_calling", "structured_output"],
            "model_family": "deepseek-v4",
            "context_window": 1000000,
            "max_output_tokens": 32768,
            "default_cost": {
                "input_per_1m_tokens": 1.667,
                "output_per_1m_tokens": 3.333,
                "cache_write_per_1m_tokens": 0.0139,
            },
        },
    ]
    extras.extend(deepseek_models)

    return extras


def _add_aliases(entries: list[dict]) -> None:
    """为已有条目添加别名映射。"""
    alias_map: dict[tuple[str, str], list[str]] = {
        # Claude: 点号 -> 连字符 映射
        ("claude", "claude-3-5-sonnet-20241022"): ["claude-3.5-sonnet-20241022"],
        ("claude", "claude-3-5-haiku-20241022"): ["claude-3.5-haiku-20241022"],
        ("claude", "claude-3-5-haiku-latest"): ["claude-3.5-haiku-latest"],
        ("claude", "claude-3-5-sonnet-20240620"): ["claude-3.5-sonnet-20240620"],
        ("claude", "claude-3-7-sonnet-20250219"): ["claude-3.7-sonnet-20250219"],
        ("claude", "claude-3-7-sonnet-latest"): ["claude-3.7-sonnet-latest"],
        # Claude: 短名 -> 带日期名
        ("claude", "claude-sonnet-4-20250514"): ["claude-sonnet-4", "claude-4-sonnet"],
        ("claude", "claude-opus-4-20250514"): ["claude-opus-4", "claude-4-opus"],
        ("claude", "claude-sonnet-4-5-20250929"): ["claude-sonnet-4.5"],
    }

    for entry in entries:
        key = (entry["endpoint_type"], entry["model"])
        if key in alias_map:
            existing = entry.get("aliases", [])
            for a in alias_map[key]:
                if a not in existing:
                    existing.append(a)
            if existing:
                entry["aliases"] = existing


def main():
    input_path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/models_dev_raw.json"
    output_path = (
        sys.argv[2] if len(sys.argv) > 2 else "internal/catalog/data/models.json"
    )

    with open(input_path, "r") as f:
        raw = json.load(f)

    entries = []
    seen_keys = set()

    for provider_id, provider in raw.items():
        if not isinstance(provider, dict):
            print(f"警告: provider '{provider_id}' 不是对象，跳过")
            continue

        endpoint_type = endpoint_type_for_provider(provider_id)

        models = provider.get("models", {})
        if not isinstance(models, dict):
            print(f"警告: provider '{provider_id}' 的 models 不是对象，跳过")
            continue

        count = 0
        for model_id, model_data in models.items():
            if not isinstance(model_data, dict):
                print(f"警告: provider '{provider_id}' 的模型 '{model_id}' 不是对象，跳过")
                continue
            entry = transform_model(provider_id, model_id, model_data)
            if entry:
                key = (entry["endpoint_type"], entry["model"])
                if key in seen_keys:
                    continue
                entries.append(entry)
                seen_keys.add(key)
                count += 1

        print(f"  {provider_id} ({endpoint_type}): {count} 个模型")

    # --- 补充 models.dev 中缺失的常见模型 ---
    existing_keys = {(e["endpoint_type"], e["model"]) for e in entries}

    supplementary = _supplementary_models()
    added = 0
    for s in supplementary:
        key = (s["endpoint_type"], s["model"])
        if key not in existing_keys:
            entries.append(s)
            existing_keys.add(key)
            added += 1

    if added:
        print(f"  补充缺失模型: {added} 个")

    # --- 为特定模型添加别名 ---
    _add_aliases(entries)

    # 按 endpoint_type + model 排序
    entries.sort(key=lambda e: (e["endpoint_type"], e["model"]))

    with open(output_path, "w") as f:
        json.dump(entries, f, indent=2, ensure_ascii=False)

    print(f"\n共生成 {len(entries)} 个模型条目 -> {output_path}")


if __name__ == "__main__":
    main()
