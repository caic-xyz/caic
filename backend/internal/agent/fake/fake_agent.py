# Fake agent that cycles through jokes, emitting Claude Code streaming JSON.
#
# Reads NDJSON from stdin (one prompt per line), responds with streaming text
# deltas followed by complete assistant and result messages. Exits on EOF.
# Used by the caic -tags e2e server for e2e testing.

import json
import sys
import time

JOKES = [
    "Why do programmers prefer dark mode? Because light attracts bugs.",
    "A SQL query walks into a bar, sees two tables, and asks: Can I JOIN you?",
    "Why do Java developers wear glasses? Because they can not C#.",
    "How many programmers does it take to change a light bulb? None, that is a hardware problem.",
    "There are only 10 types of people: those who understand binary and those who do not.",
    "A programmer puts two glasses on his bedside table before going to sleep."
    " A full one, in case he gets thirsty, and an empty one, in case he does not.",
]

PLAN_CONTENT = "1. Analyze the problem\n2. Implement the solution\n"

ASK_QUESTION = {
    "question": "Which approach should I use?",
    "options": [
        {"label": "Option A", "description": "Fast but less robust"},
        {"label": "Option B", "description": "Slower but more robust"},
    ],
}


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def emit_tool_use(tool_id: str, name: str, input_obj: dict) -> None:
    emit(
        {
            "type": "assistant",
            "message": {
                "role": "assistant",
                "content": [{"type": "tool_use", "id": tool_id, "name": name, "input": input_obj}],
            },
        }
    )


def emit_result(turns: int, result: str) -> None:
    emit(
        {
            "type": "result",
            "subtype": "success",
            "result": result,
            "num_turns": turns,
            "total_cost_usd": 0.01,
            "duration_ms": 500,
        }
    )


def emit_plan_turn(turns: int) -> None:
    """Emit Write(.claude/plans/plan.md) + ExitPlanMode + result."""
    emit_tool_use(
        "toolu_write_plan",
        "Write",
        {"file_path": ".claude/plans/plan.md", "content": PLAN_CONTENT},
    )
    emit_tool_use("toolu_exit_plan", "ExitPlanMode", {})
    emit_result(turns, "Plan created")


def emit_ask_turn(turns: int) -> None:
    """Emit AskUserQuestion + result."""
    emit_tool_use(
        "toolu_ask",
        "AskUserQuestion",
        {"questions": [ASK_QUESTION]},
    )
    emit_result(turns, "Asking user")


def main() -> None:
    # System init before first prompt.
    emit(
        {
            "type": "system",
            "subtype": "init",
            "session_id": "test-session",
            "cwd": "/workspace",
            "model": "fake-model",
            "claude_code_version": "0.0.0-test",
        }
    )

    turns = 0
    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line:
            continue
        turns += 1

        if "FAKE_PLAN" in line:
            emit_plan_turn(turns)
            continue

        if "FAKE_ASK" in line:
            emit_ask_turn(turns)
            continue

        joke = JOKES[(turns - 1) % len(JOKES)]

        # Split roughly in half for two streaming deltas.
        mid = len(joke) // 2
        # Advance to the next space so we don't split mid-word.
        sp = joke.find(" ", mid)
        if sp == -1:
            sp = mid
        part1 = joke[: sp + 1]
        part2 = joke[sp + 1 :]

        emit(
            {
                "type": "stream_event",
                "event": {
                    "type": "content_block_delta",
                    "index": 0,
                    "delta": {"type": "text_delta", "text": part1},
                },
            }
        )
        time.sleep(0.05)
        emit(
            {
                "type": "stream_event",
                "event": {
                    "type": "content_block_delta",
                    "index": 0,
                    "delta": {"type": "text_delta", "text": part2},
                },
            }
        )
        emit(
            {
                "type": "assistant",
                "message": {
                    "role": "assistant",
                    "content": [{"type": "text", "text": joke}],
                },
            }
        )
        emit(
            {
                "type": "result",
                "subtype": "success",
                "result": joke,
                "num_turns": turns,
                "total_cost_usd": 0.01,
                "duration_ms": 500,
            }
        )


if __name__ == "__main__":
    main()
