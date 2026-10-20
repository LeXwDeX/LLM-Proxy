#!/usr/bin/env python3
import argparse
import json
import urllib.error
import urllib.request
import sys
import time
import urllib.parse
from collections import defaultdict
from pathlib import Path


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Preserve the legacy curl behavior and never forward credentials to
        # a redirected host. Redirect status is a failed availability probe.
        return None


def load_config(path: Path) -> dict:
    try:
        return json.loads(path.read_text())
    except Exception as exc:
        raise SystemExit(f"failed to read config {path}: {exc}")


def probe(target_name: str, endpoint: str, api_key: str, model: str, connect_timeout: int, max_time: int):
    url = f"{endpoint.rstrip('/')}/openai/v1/models/{urllib.parse.quote(model, safe='')}"
    # connect_timeout remains in the legacy signature; urllib uses max_time
    # for socket operations. Credentials stay in the in-process request header.
    request = urllib.request.Request(url, headers={"api-key": api_key})
    start = time.monotonic()
    try:
        with urllib.request.build_opener(_NoRedirect).open(request, timeout=max_time) as response:
            status = str(response.status)
        ok = status == "200"
        detail = f"http_{status}"
    except urllib.error.HTTPError as error:
        status = str(error.code)
        ok = False
        detail = f"http_{status}"
        error.close()
    except Exception as error:
        status = "transport_error"
        ok = False
        detail = type(error).__name__
    elapsed_ms = int((time.monotonic() - start) * 1000)

    return {
        "target": target_name,
        "model": model,
        "status": status,
        "ok": ok,
        "elapsed_ms": elapsed_ms,
        "detail": detail,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Verify Azure OpenAI v1 model availability by config")
    parser.add_argument("--config", default="config/config.json", help="Path to config json")
    parser.add_argument("--connect-timeout", type=int, default=3, help="Deprecated compatibility flag; --max-time controls socket timeout")
    parser.add_argument("--max-time", type=int, default=5, help="HTTP socket timeout seconds")
    args = parser.parse_args()

    config_path = Path(args.config)
    cfg = load_config(config_path)

    rows = []
    for target in cfg.get("azure_targets", []):
        tname = (target.get("name") or "").strip()
        endpoint = (target.get("endpoint") or "").strip()
        api_key = target.get("azure_api_key") or ""
        models = target.get("allowed_models") or []

        for model in models:
            model = (model or "").strip()
            if not model:
                continue
            rows.append(probe(tname, endpoint, api_key, model, args.connect_timeout, args.max_time))

    if not rows:
        print("No Azure models were probed; this legacy config verifier cannot verify database-backed targets.", file=sys.stderr)
        return 2

    summary = defaultdict(lambda: {"ok": 0, "fail": 0})
    for row in rows:
        if row["ok"]:
            summary[row["target"]]["ok"] += 1
        else:
            summary[row["target"]]["fail"] += 1

    print("=== SUMMARY ===")
    for target in sorted(summary):
        item = summary[target]
        print(f"{target}: ok={item['ok']} fail={item['fail']}")

    print("\n=== DETAILS ===")
    for row in rows:
        mark = "OK" if row["ok"] else "FAIL"
        print(f"[{mark}] {row['target']} | {row['model']} | {row['status']} | {row['elapsed_ms']}ms | {row['detail']}")

    has_fail = any(not row["ok"] for row in rows)
    return 1 if has_fail else 0


if __name__ == "__main__":
    sys.exit(main())
