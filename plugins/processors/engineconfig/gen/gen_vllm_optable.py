#!/usr/bin/env python3
"""Generate optable.go from the OPTIONS block of the reference parser parse_vllm.py (vLLM v0.32.0).

Usage: gen_vllm_optable.py parse_vllm.py > ../optable.go; gofmt -w ../optable.go
"""
import re, sys
src = open(sys.argv[1]).read()
blk = src[src.index('# BEGIN OPTIONS'):src.index('# END OPTIONS')]
rows = []
for m in re.finditer(r"'(-[^']+)': \('([^']+)', ('?[^,]+'?), '([^']+)'\)", blk):
    opt, dest, nargs, kind = m.groups()
    nargs = {"0": "nargsZero", "1": "nargsOne", "'+'": "nargsPlus", "'?'": "nargsOptional"}[nargs]
    neg = "true" if kind == "bool_optional_neg" else "false"
    rows.append((opt, dest, nargs, neg))
# Flags removed before v0.32.0 that older images still pass. Listed so that the
# value after them is not taken for a positional, and the speculative ones are read.
legacy = [
    ("--speculative-model", "legacy_speculative_model", "nargsOne", "false"),
    ("--num-speculative-tokens", "legacy_num_speculative_tokens", "nargsOne", "false"),
    ("--disable-log-requests", "disable_log_requests", "nargsZero", "false"),
    ("--use-v2-block-manager", "use_v2_block_manager", "nargsZero", "false"),
    ("--swap-space", "swap_space", "nargsOne", "false"),
]
have = {r[0] for r in rows}
for r in legacy:
    if r[0] not in have:
        rows.append(r)
rows.sort()
out = ["// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.",
       "// SPDX-License-Identifier: MIT",
       "",
       "package engineconfig",
       "",
       "// vllmOptions is the vLLM v0.32.0 option table (EngineArgs.add_cli_args and",
       "// make_arg_parser), taken from the reference parser parse_vllm.py, plus a few flags",
       "// that older releases still accept (--speculative-model, --num-speculative-tokens,",
       "// --disable-log-requests, --use-v2-block-manager, --swap-space).",
       "var vllmOptions = map[string]option{"]
for opt, dest, nargs, neg in rows:
    neg_s = ", neg: true" if neg == "true" else ""
    out.append('\t"%s": {dest: "%s", nargs: %s%s},' % (opt, dest, nargs, neg_s))
out.append("}")
print("\n".join(out))
