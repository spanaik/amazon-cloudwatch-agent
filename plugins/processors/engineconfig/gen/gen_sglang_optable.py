#!/usr/bin/env python3
"""Generate optable_sglang.go from the SGLang server arguments.

Usage: gen_sglang_optable.py <sglang clone>/python/sglang/srt > ../optable_sglang.go; gofmt -w ../optable_sglang.go

SGLang v0.5.22 (commit baa9befa38) builds its parser in ServerArgs.add_cli_args
(server_args.py): add_cli_args_from_dataclass registers every A[T, ...] field of
the classes in _INPUT_NAMESPACES (arg_groups/fields/*.py), then add_cli_args adds
a few flags by hand. This script reads both with ast, without importing SGLang
(which needs msgspec and torch), and applies the rules of
arg_utils.add_cli_args_from_dataclass to get each flag's dest and nargs.
"""
import ast
import pathlib
import sys

srt = pathlib.Path(sys.argv[1])
server_args = ast.parse((srt / "server_args.py").read_text())

namespaces = None
for node in server_args.body:
    if isinstance(node, ast.Assign) and any(getattr(t, "id", "") == "_INPUT_NAMESPACES" for t in node.targets):
        namespaces = [e.id for e in node.value.elts]
assert namespaces, "no _INPUT_NAMESPACES in server_args.py"
classes = {}
for f in sorted((srt / "arg_groups" / "fields").glob("*.py")):
    for node in ast.parse(f.read_text()).body:
        if isinstance(node, ast.ClassDef):
            classes[node.name] = node

NARGS = {None: "nargsOne", "?": "nargsOptional", "+": "nargsPlus", "*": "nargsStar", 0: "nargsZero"}
ZERO_ACTIONS = {"store_true", "store_false", "store_const", "DeprecatedStoreTrueAction",
                "DeprecatedStoreConstAction", "DeprecatedAction"}
rows = {}  # option string -> (dest, nargs, neg)


def add(opt, dest, nargs, neg=False):
    row = (dest, NARGS[nargs], neg)
    assert rows.get(opt, row) == row, (opt, rows[opt], row)
    rows[opt] = row


def name_of(node):
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return node.attr
    if isinstance(node, ast.Constant):
        return node.value
    return None


def strip_optional(t):
    """Optional[X], Union[X, None] and X | None give X."""
    if isinstance(t, ast.Subscript) and name_of(t.value) == "Optional":
        return t.slice
    if isinstance(t, ast.Subscript) and name_of(t.value) == "Union":
        elts = [e for e in t.slice.elts if not (isinstance(e, ast.Constant) and e.value is None)]
        return elts[0] if len(elts) == 1 else t
    if isinstance(t, ast.BinOp) and isinstance(t.right, ast.Constant) and t.right.value is None:
        return t.left
    return t


def register(names, dest, action, nargs, action_kwargs):
    """The nargs that argparse gives each option string."""
    if action == "BooleanOptionalAction":
        for n in names:
            add(n, dest, 0)
            if n.startswith("--"):
                add("--no-" + n[2:], dest, 0, neg=True)
        return
    if action in ZERO_ACTIONS:
        nargs = 0
    elif action is not None:
        nargs = action_kwargs.get("nargs")
    for n in names:
        add(n, dest, nargs, neg=action == "store_false")


# 1. add_cli_args_from_dataclass over the input namespaces
for cls in namespaces:
    for stmt in classes[cls].body:
        if not (isinstance(stmt, ast.AnnAssign) and isinstance(stmt.target, ast.Name)):
            continue
        ann = stmt.annotation
        if not (isinstance(ann, ast.Subscript) and name_of(ann.value) in ("A", "Annotated")):
            continue
        typ, *meta = ann.slice.elts
        arg = {}
        if any(isinstance(m, ast.Constant) and isinstance(m.value, str) for m in meta):
            arg = {"help": True}
        for m in meta:
            if isinstance(m, ast.Call) and name_of(m.func) == "Arg":
                arg = {k.arg: k.value for k in m.keywords}
        if not arg or (isinstance(arg.get("no_cli"), ast.Constant) and arg["no_cli"].value):
            continue
        field = stmt.target.id
        cli = arg["cli_name"].value if "cli_name" in arg else "--" + field.replace("_", "-")
        names = [cli] + [e.value for e in arg["aliases"].elts] if "aliases" in arg else [cli]
        nargs = arg["nargs"].value if "nargs" in arg else None
        kwargs = {}
        if "action_kwargs" in arg:
            kwargs = {k.value: name_of(v) for k, v in zip(arg["action_kwargs"].keys, arg["action_kwargs"].values)}
        if "action" in arg:
            register(names, field, name_of(arg["action"]), nargs, kwargs)
            continue
        inner = strip_optional(typ)
        head = name_of(inner.value) if isinstance(inner, ast.Subscript) else None
        if head == "Literal":
            register(names, field, None, None, {})
        elif head in ("List", "list") and "type_parser" not in arg:
            register(names, field, None, nargs or "+", {})
        elif name_of(inner) == "bool":
            register(names, field, "store_true", None, {})
        else:
            register(names, field, None, nargs, {})

# 2. the flags that ServerArgs.add_cli_args adds by hand, and argparse's own -h
add("-h", "help", 0)
add("--help", "help", 0)
server_cls = next(n for n in server_args.body if isinstance(n, ast.ClassDef) and n.name == "ServerArgs")
add_cli = next(n for n in server_cls.body if isinstance(n, ast.FunctionDef) and n.name == "add_cli_args")
for call in ast.walk(add_cli):
    if not (isinstance(call, ast.Call) and name_of(call.func) == "add_argument"):
        continue
    names = [a.value for a in call.args]
    kw = {k.arg: k.value for k in call.keywords}
    long = next(n for n in names if n.startswith("--"))
    dest = kw["dest"].value if "dest" in kw else long[2:].replace("-", "_")
    register(names, dest, name_of(kw["action"]) if "action" in kw else None,
             kw["nargs"].value if "nargs" in kw else None, {})

out = ["// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.",
       "// SPDX-License-Identifier: MIT",
       "",
       "// Code generated by gen/gen_sglang_optable.py from SGLang v0.5.22 (commit",
       "// baa9befa38), python/sglang/srt/server_args.py and arg_groups/fields. DO NOT EDIT.",
       "",
       "package engineconfig",
       "",
       "// sglangOptions is the SGLang v0.5.22 option table (ServerArgs.add_cli_args).",
       "var sglangOptions = map[string]option{"]
for opt in sorted(rows):
    dest, nargs, neg = rows[opt]
    out.append('\t"%s": {dest: "%s", nargs: %s%s},' % (opt, dest, nargs, ", neg: true" if neg else ""))
out.append("}")
print("\n".join(out))
