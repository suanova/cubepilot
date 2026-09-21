#!/usr/bin/env python3
"""把 /api/v1/sessions/{key}/messages 的原始 SSE 流转成可读的回放。

在 Bruno 里把响应面板的内容复制出来存成文件,然后:

    python3 scripts/decode-sse.py turn.sse

也可以直接从命令行喂:

    curl -sN ... -d '{...}' | python3 scripts/decode-sse.py -
"""

import json
import re
import sys

C = {
    "dim": "\033[2m", "b": "\033[1m", "r": "\033[0m",
    "blue": "\033[34m", "green": "\033[32m", "red": "\033[31m",
    "yellow": "\033[33m", "cyan": "\033[36m", "magenta": "\033[35m",
}

# Everything this script prints from the stream -- model text, tool arguments,
# tool output, event names, error strings -- is attacker-influenced: a run that
# reads hostile content, or a model that emits raw bytes, controls those
# strings. Printed as-is they can drive the terminal (CWE-150): move the
# cursor, rewrite this script's own output, set the title, or worse on a
# terminal that implements more of the escape set. C's color codes above are
# ours and stay outside this.
_CTRL = re.compile(
    r"\x1b\[[0-9;?]*[ -/]*[@-~]"   # CSI ... final byte
    r"|\x1b[\]P^_][^\x07\x1b]*(?:\x07|\x1b\\)"  # OSC / DCS / PM / APC string
    r"|\x1b[@-Z\\-_]"              # two-char escapes
    r"|[\x00-\x08\x0b-\x1f\x7f-\x9f]"          # remaining C0 / DEL / C1
)


def safe(value):
    """Neutralize terminal control sequences in an untrusted string."""
    return _CTRL.sub("", str(value))


def frames(text):
    """Yield (event_name, data_dict) for each complete SSE frame."""
    event, data = None, []
    for line in text.splitlines():
        if line.startswith("event:"):
            event = line[6:].strip()
        elif line.startswith("data:"):
            data.append(line[5:].strip())
        elif line == "":
            if data:
                raw = "\n".join(data)
                try:
                    yield event, json.loads(raw)
                except json.JSONDecodeError:
                    yield event, {"_raw": raw}
            event, data = None, []


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else "-"
    text = sys.stdin.read() if src == "-" else open(src, encoding="utf-8").read()

    display = ""       # 用户此刻应该看到的文本
    reasoning = ""     # 已被 text_replace 丢弃的推理
    session = ""
    tool_names = {}    # callId -> name
    counts = {}
    outcome = None

    def rule(label, color=C["dim"]):
        print(f"{color}── {label} {'─' * max(0, 58 - len(label))}{C['r']}")

    print()
    for name, d in frames(text):
        counts[name] = counts.get(name, 0) + 1
        t = d.get("type", name)

        if t == "message_start":
            session = d.get("sessionId", "")
            rule("回合开始", C["blue"])
            print(f"  sessionId: {C['b']}{safe(session)}{C['r']}")

        elif t == "message_delta":
            display += d.get("delta", "")

        elif t == "text_replace":
            # 关键:这是**替换**。之前累积的 text 作废 ——
            # 网关在工具执行后重写了先前的解说文本(模型常把推理也吐进 delta)。
            if display and display != d.get("delta"):
                reasoning += display
            display = d.get("delta", "")
            print(f"\n  {C['yellow']}[text_replace]{C['r']} {C['dim']}整段替换{C['r']}")
            if reasoning:
                print(f"  {C['dim']}丢弃了 {len(reasoning)} 字符 —— "
                      f"前端若把它当 message_delta 追加,这些就会留在界面上:{C['r']}")
                print(f"  {C['dim']}{safe(reasoning.strip()[:300])}"
                      f"{'…' if len(reasoning.strip()) > 300 else ''}{C['r']}")

        elif t == "tool_call":
            tool_names[d.get("callId", "")] = d.get("name", "?")
            if reasoning:
                rule("被丢弃的推理(前端不该显示)", C["dim"])
                print(f"  {C['dim']}{safe(reasoning[:400])}{'…' if len(reasoning) > 400 else ''}{C['r']}")
                reasoning = ""
            rule("工具调用", C["magenta"])
            print(f"  {C['magenta']}→{C['r']} {C['b']}{safe(d.get('name', '?'))}{C['r']}  "
                  f"{C['dim']}{safe(d.get('arguments', '')[:160])}{C['r']}")

        elif t == "tool_result":
            out = (d.get("output") or "").strip().replace("\n", " ")[:160]
            color = C["red"] if "denied" in out.lower() else C["dim"]
            print(f"  {C['magenta']}←{C['r']} {color}{safe(out)}{C['r']}")

        elif t in ("approval_pending", "question_pending"):
            rule("等待人工输入 —— 回合在此暂停", C["yellow"])
            print(f"  {C['yellow']}{safe(json.dumps(d, ensure_ascii=False))}{C['r']}")
            print(f"  {C['dim']}需要另外发一条请求才能继续"
                  f"(/approval 或 /question);用完整 canonical key。{C['r']}")

        elif t in ("approval_resolved", "question_resolved"):
            print(f"  {C['green']}✓{C['r']} {safe(t)}: {safe(json.dumps(d, ensure_ascii=False))}")

        elif t == "message_done":
            outcome = d

    rule("最终回复(用户实际看到的)", C["green"])
    body = display.strip()
    if body:
        for line in body.splitlines() or [""]:
            print(f"  {C['b']}{safe(line)}{C['r']}")
    else:
        print(f"  {C['dim']}(空){C['r']}")

    rule("终态", C["blue"])
    if outcome is None:
        print(f"  {C['red']}没有收到 message_done —— 流被提前切断了。{C['r']}")
        print(f"  {C['dim']}客户端必须自己合成一个终态让 UI 复位,否则会永远卡在「进行中」。{C['r']}")
    elif outcome.get("error"):
        print(f"  {C['red']}error: {safe(outcome['error'])}{C['r']}")
    elif outcome.get("stopped"):
        print(f"  {C['yellow']}stopped: true(被 /abort 停止){C['r']}")
    else:
        print(f"  {C['green']}正常结束{C['r']}")

    print()
    rule("帧统计", C["dim"])
    for k, v in sorted(counts.items(), key=lambda kv: -kv[1]):
        print(f"  {v:>3}  {safe(k)}")
    print()


if __name__ == "__main__":
    main()
