#!/usr/bin/env python3
"""Run the RV idle driver through the kernel's actual s2idle selection path."""
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
KERNEL = ROOT / "pico-sdk/sysdrv/source/kernel"


def function(source, name):
    start = re.search(r"^(?:static )?(?:int|void) " + name + r"\(", source, re.M).start()
    opening = source.index("{", start)
    depth = 1
    end = opening + 1
    while depth:
        depth += (source[end] == "{") - (source[end] == "}")
        end += 1
    return source[start:end]


with tempfile.TemporaryDirectory() as tmp:
    tmp = Path(tmp)
    driver = (KERNEL / "drivers/cpuidle/cpuidle-rv1106.c").read_text()
    driver = re.sub(r"^#include.*$", "", driver, flags=re.M)
    core = (KERNEL / "drivers/cpuidle/cpuidle.c").read_text()
    extracted = "\n".join(function(core, name) for name in (
        "find_deepest_state", "enter_s2idle_proper", "cpuidle_enter_s2idle"))
    harness = (ROOT / "tests/rv1106_s2idle_harness.c").read_text()
    generated = tmp / "s2idle.c"
    generated.write_text(harness.replace("/* PRODUCTION_CODE */", driver + "\n" + extracted))
    binary = tmp / "s2idle"
    subprocess.run(["gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
                    "-Wno-unused-parameter", str(generated), "-o", str(binary)], check=True)
    subprocess.run([str(binary)], check=True)
