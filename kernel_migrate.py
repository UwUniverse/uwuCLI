#!/usr/bin/env python3

# Copyright (C) 2026 The uwuAOSP Project
# SPDX-License-Identifier: Apache-2.0

"""Convert legacy uwuAOSP kernel BoardConfig settings to uwu_kernel."""

from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path


ASSIGNMENT = re.compile(r"^\s*([A-Za-z0-9_]+)\s*(:=|\?=|\+=|=)\s*(.*?)\s*$")
INCLUDE = re.compile(r"^\s*-?include\s+(.+?)\s*$")
REMOVED_VARIABLES = {
    "TARGET_KERNEL_SOURCE",
    "TARGET_KERNEL_ARCH",
    "TARGET_KERNEL_CLANG_VERSION",
    "TARGET_KERNEL_CLANG_PATH",
    "TARGET_KERNEL_RUST_VERSION",
    "TARGET_KERNEL_NO_GCC",
    "TARGET_KERNEL_CONFIG",
    "TARGET_KERNEL_CONFIG_EXT",
    "TARGET_KERNEL_ADDITIONAL_FLAGS",
    "TARGET_KERNEL_EXT_MODULE_ROOT",
    "TARGET_KERNEL_EXT_MODULES",
    "MERGE_ALL_KERNEL_CONFIGS_AT_ONCE",
    "KERNEL_LTO",
    "KERNEL_CONFIG_OVERRIDE",
    "BOARD_USES_QCOM_MERGE_DTBS_SCRIPT",
    "BOARD_SYSTEM_KERNEL_MODULES_LOAD",
    "BOARD_VENDOR_KERNEL_MODULES_BLOCKLIST_FILE",
    "BOARD_VENDOR_KERNEL_MODULES_LOAD",
    "BOARD_VENDOR_RAMDISK_KERNEL_MODULES_BLOCKLIST_FILE",
    "BOARD_VENDOR_RAMDISK_KERNEL_MODULES_LOAD",
    "BOARD_VENDOR_RAMDISK_RECOVERY_KERNEL_MODULES_LOAD",
    "BOOT_KERNEL_MODULES",
    "SYSTEM_KERNEL_MODULES",
}


@dataclass
class Migration:
    device_dir: Path
    board_config: Path
    android_bp: Path
    product_mk: Path
    variables: dict[str, str]
    block: str
    inferred_kernel_version: str = ""
    blockers: list[str] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)

    @property
    def module_label(self) -> str:
        return f"//{self.device_dir.as_posix()}:kernel"


def logical_lines(text: str) -> list[str]:
    result: list[str] = []
    current = ""
    for raw in text.splitlines():
        line = raw.rstrip()
        if current:
            current += " " + line.lstrip()
        else:
            current = line
        if current.endswith("\\"):
            current = current[:-1].rstrip()
            continue
        result.append(current)
        current = ""
    if current:
        result.append(current)
    return result


def parse_variables(text: str) -> dict[str, str]:
    values: dict[str, str] = {}
    for line in logical_lines(text):
        match = ASSIGNMENT.match(line)
        if match:
            update_variable(values, match.group(1), match.group(2), match.group(3).strip())
    return values


def update_variable(values: dict[str, str], name: str, operator: str, value: str) -> None:
    if operator == "?=" and name in values:
        return
    if operator == "+=" and name in values:
        values[name] = f"{values[name]} {value}".strip()
    else:
        values[name] = value


def parse_board_config(path: Path, source_root: Path, values: dict[str, str] | None = None, visited: set[Path] | None = None) -> dict[str, str]:
    values = values if values is not None else {}
    visited = visited if visited is not None else set()
    path = path.resolve()
    if path in visited:
        return values
    visited.add(path)
    for line in logical_lines(path.read_text()):
        assignment = ASSIGNMENT.match(line)
        if assignment:
            update_variable(values, assignment.group(1), assignment.group(2), assignment.group(3).strip())
            continue
        include = INCLUDE.match(line)
        if not include:
            continue
        include_path = expand_make_variables(include.group(1), values)
        if "$" in include_path:
            continue
        candidate = Path(include_path)
        if not candidate.is_absolute():
            candidate = source_root / candidate
        if candidate.is_file() and candidate.is_relative_to(source_root):
            parse_board_config(candidate, source_root, values, visited)
    return values


def words(value: str) -> list[str]:
    return [item for item in value.split() if item]


def unique(items: list[str]) -> list[str]:
    return list(dict.fromkeys(items))


def bp_list(items: list[str], indent: int = 8) -> str:
    if not items:
        return "[]"
    prefix = " " * indent
    return "[\n" + "".join(f'{prefix}"{item}",\n' for item in items) + " " * (indent - 4) + "]"


def resolve_make_path(path: str, device_dir: Path, variables: dict[str, str], source_root: Path) -> Path:
    common_path = variables.get("COMMON_PATH", device_dir.as_posix())
    path = path.replace("$(COMMON_PATH)", common_path)
    path = path.replace("$(TARGET_KERNEL_SOURCE)", variables.get("TARGET_KERNEL_SOURCE", ""))
    candidate = Path(path)
    if not candidate.is_absolute():
        candidate = source_root / candidate
    return candidate


def expand_make_variables(value: str, variables: dict[str, str]) -> str:
    for _ in range(16):
        expanded = re.sub(r"\$\(([A-Za-z0-9_]+)\)", lambda match: variables.get(match.group(1), match.group(0)), value)
        if expanded == value:
            return expanded
        value = expanded
    return value


def cat_files(value: str, device_dir: Path, variables: dict[str, str], source_root: Path) -> list[str] | None:
    paths = cat_source_paths(value, device_dir, variables, source_root)
    if paths is None:
        return words(value) if "$" not in value else None
    result: list[str] = []
    for path in paths:
        result.extend(line.strip() for line in path.read_text().splitlines() if line.strip() and not line.lstrip().startswith("#"))
    return unique(result)


def cat_paths(value: str, device_dir: Path, variables: dict[str, str], source_root: Path) -> list[str] | None:
    paths = cat_source_paths(value, device_dir, variables, source_root)
    if paths is None:
        return None
    return [path.relative_to(source_root).as_posix() for path in paths]


def cat_source_paths(value: str, device_dir: Path, variables: dict[str, str], source_root: Path) -> list[Path] | None:
    marker = "$(shell cat "
    start = value.find(marker)
    if start < 0:
        return None
    depth = 0
    end = -1
    index = start
    while index < len(value):
        if value.startswith("$(", index):
            depth += 1
            index += 2
            continue
        if value[index] == ")":
            depth -= 1
            if depth == 0:
                end = index
                break
        index += 1
    if end < 0:
        return None
    arguments = value[start + len(marker):end]
    result: list[Path] = []
    for item in words(arguments):
        path = resolve_make_path(item, device_dir, variables, source_root)
        if not path.is_file():
            return None
        result.append(path)
    return result


def add_list(lines: list[str], name: str, values: list[str], indent: int = 8) -> None:
    if values:
        lines.append(f'{" " * indent}{name}: {bp_list(values, indent + 4)},')


def build_module(device_dir: Path, variables: dict[str, str], source_root: Path) -> tuple[str, list[str], list[str]]:
    blockers: list[str] = []
    warnings: list[str] = []
    kernel_dir = variables.get("TARGET_KERNEL_SOURCE", "")
    if not kernel_dir:
        blockers.append("TARGET_KERNEL_SOURCE is missing")
    configs = words(variables.get("TARGET_KERNEL_CONFIG", ""))
    if not configs:
        blockers.append("TARGET_KERNEL_CONFIG is missing")
    image_name = variables.get("BOARD_KERNEL_IMAGE_NAME", "")
    if not image_name:
        blockers.append("BOARD_KERNEL_IMAGE_NAME is missing")
    arch = variables.get("TARGET_KERNEL_ARCH", variables.get("TARGET_ARCH", ""))
    if not arch:
        blockers.append("kernel architecture could not be inferred")

    if variables.get("TARGET_KERNEL_PLATFORM_TARGET"):
        blockers.append("TARGET_KERNEL_PLATFORM_TARGET (Kleaf) is not supported by uwu_kernel source builds")

    list_vars: dict[str, list[str]] = {}
    list_paths: dict[str, list[str]] = {}
    for name in (
        "SYSTEM_KERNEL_MODULES",
        "BOARD_SYSTEM_KERNEL_MODULES_LOAD",
        "BOARD_VENDOR_KERNEL_MODULES_LOAD",
        "BOOT_KERNEL_MODULES",
        "BOARD_VENDOR_RAMDISK_KERNEL_MODULES_LOAD",
        "BOARD_VENDOR_RAMDISK_RECOVERY_KERNEL_MODULES_LOAD",
    ):
        value = variables.get(name, "")
        if not value:
            continue
        parsed = cat_files(value, device_dir, variables, source_root)
        paths = cat_paths(value, device_dir, variables, source_root)
        if parsed is None or paths is None:
            blockers.append(f"{name} must reference one or more static module list files")
        else:
            list_vars[name] = parsed
            list_paths[name] = paths

    lines = ["uwu_kernel {", '    name: "kernel",']
    if kernel_dir:
        lines.append(f'    kernel_dir: "{kernel_dir}",')
    if arch:
        lines.append(f'    kernel_arch: "{arch}",')
    if image_name:
        lines.append(f'    image_name: "{image_name}",')
    clang = variables.get("TARGET_KERNEL_CLANG_VERSION", "")
    if clang:
        if not clang.startswith("clang-"):
            clang = "clang-" + clang
    else:
        clang = "clang-r563880c"
    lines.append(f'    clang_version: "{clang}",')
    clang_path = variables.get("TARGET_KERNEL_CLANG_PATH", "")
    if clang_path:
        lines.append(f'    clang_path: "{clang_path}",')
    rust = variables.get("TARGET_KERNEL_RUST_VERSION", "")
    if rust:
        lines.append(f'    rust_version: "{rust}",')
    additional = words(variables.get("TARGET_KERNEL_ADDITIONAL_FLAGS", ""))
    add_list(lines, "additional_flags", additional, 4)

    if configs:
        lines.extend(["", "    config: {", f'        defconfig: "{configs[0]}",'])
        add_list(lines, "fragments", configs[1:], 8)
        if variables.get("MERGE_ALL_KERNEL_CONFIGS_AT_ONCE") == "true":
            lines.append("        merge_at_once: true,")
        lto = variables.get("KERNEL_LTO", "")
        if lto:
            lines.append(f'        lto: "{lto.lower()}",')
        lines.append("    },")

    qcom_merge = variables.get("BOARD_USES_QCOM_MERGE_DTBS_SCRIPT") == "true"
    if variables.get("BOARD_INCLUDE_DTB_IN_BOOTIMG") == "true":
        lines.extend(["", "    dtb: {", "        enabled: true,"])
        if qcom_merge:
            lines.append("        qcom_merge: true,")
        lines.extend(['        target: "dtbs",', '        image_name: "dtb.img",', "    },"])
    if variables.get("TARGET_NEEDS_DTBOIMAGE") == "true" or variables.get("BOARD_KERNEL_SEPARATED_DTBO") == "true":
        lines.extend(["", "    dtbo: {", "        enabled: true,", '        target: "dtbs",', '        image_name: "dtbo.img",'])
        page_size = variables.get("BOARD_KERNEL_PAGESIZE", "")
        if page_size.isdigit():
            lines.append(f"        page_size: {page_size},")
        lines.append("    },")

    external = words(variables.get("TARGET_KERNEL_EXT_MODULES", ""))
    module_values = any(list_vars.values()) or external
    if module_values:
        lines.extend(["", "    modules: {", "        enabled: true,"])
        root = variables.get("TARGET_KERNEL_EXT_MODULE_ROOT", "")
        if external and not root:
            blockers.append("TARGET_KERNEL_EXT_MODULES is set without TARGET_KERNEL_EXT_MODULE_ROOT")
        if root:
            lines.append(f'        external_module_root: "{root}",')
        add_list(lines, "external_modules", external, 8)
        system_install = list_paths.get("SYSTEM_KERNEL_MODULES")
        system_load = list_paths.get("BOARD_SYSTEM_KERNEL_MODULES_LOAD")
        vendor_load = list_paths.get("BOARD_VENDOR_KERNEL_MODULES_LOAD")
        ramdisk_install = list_paths.get("BOOT_KERNEL_MODULES")
        ramdisk_load = list_paths.get("BOARD_VENDOR_RAMDISK_KERNEL_MODULES_LOAD")
        recovery_load = cat_paths(variables.get("BOARD_VENDOR_RAMDISK_RECOVERY_KERNEL_MODULES_LOAD", ""), device_dir, variables, source_root)
        recovery_install = recovery_load

        for partition, install, load in (
            ("system_dlkm", system_install, system_load),
            ("vendor_dlkm", vendor_load, vendor_load),
            ("vendor_ramdisk", ramdisk_install, ramdisk_load),
            ("recovery", recovery_install, recovery_load),
        ):
            if bool(install) != bool(load):
                blockers.append(f"{partition} requires both install and load list files")
        add_list(lines, "system_dlkm_module_install_list", system_install or [], 8)
        add_list(lines, "system_dlkm_module_load_list", system_load or [], 8)
        add_list(lines, "vendor_dlkm_module_install_list", vendor_load or [], 8)
        add_list(lines, "vendor_dlkm_module_load_list", vendor_load or [], 8)
        add_list(lines, "vendor_ramdisk_module_install_list", ramdisk_install or [], 8)
        add_list(lines, "vendor_ramdisk_module_load_list", ramdisk_load or [], 8)
        add_list(lines, "recovery_module_install_list", recovery_install or [], 8)
        add_list(lines, "recovery_module_load_list", recovery_load or [], 8)
        vendor_blocklist = expand_make_variables(variables.get("BOARD_VENDOR_KERNEL_MODULES_BLOCKLIST_FILE", ""), variables)
        ramdisk_blocklist = expand_make_variables(variables.get("BOARD_VENDOR_RAMDISK_KERNEL_MODULES_BLOCKLIST_FILE", ""), variables)
        if vendor_blocklist:
            lines.append(f'        vendor_dlkm_module_blocklist: "{vendor_blocklist}",')
        if ramdisk_blocklist:
            lines.append(f'        vendor_ramdisk_module_blocklist: "{ramdisk_blocklist}",')
            lines.append(f'        recovery_module_blocklist: "{ramdisk_blocklist}",')
        lines.append("        auto_collect_deps: true,")
        lines.append("    },")
    lines.append("}")
    return "\n".join(lines) + "\n", blockers, warnings


def locate_board_config(device_dir: Path, explicit: str | None) -> Path:
    if explicit:
        return Path(explicit)
    common = device_dir / "BoardConfigCommon.mk"
    if common.is_file():
        return common
    board = device_dir / "BoardConfig.mk"
    if board.is_file():
        return board
    raise ValueError("no BoardConfigCommon.mk or BoardConfig.mk found")


def locate_device_dir(source_root: Path, device: str) -> Path:
    candidate = Path(device)
    if candidate.is_absolute() or "/" in device:
        return candidate if candidate.is_absolute() else source_root / candidate
    matches = sorted(path for path in (source_root / "device").glob(f"*/{device}") if path.is_dir())
    if not matches:
        raise ValueError(f"no device tree found for {device}")
    if len(matches) > 1:
        raise ValueError(f"multiple device trees found for {device}; specify --device-dir")
    return matches[0]


def infer_kernel_version(source_root: Path, kernel_dir: str) -> str:
    makefile = source_root / kernel_dir / "Makefile"
    if not makefile.is_file():
        return ""
    values = parse_variables(makefile.read_text())
    version = values.get("VERSION", "")
    patchlevel = values.get("PATCHLEVEL", "")
    if version.isdigit() and patchlevel.isdigit():
        return f"{version}.{patchlevel}"
    return ""


def analyze(source_root: Path, device_dir_arg: str, board_config_arg: str | None) -> Migration:
    device_abs = locate_device_dir(source_root, device_dir_arg)
    device_abs = device_abs.resolve()
    try:
        device_rel = device_abs.relative_to(source_root)
    except ValueError as error:
        raise ValueError("device directory must be inside the Android source tree") from error
    board_config = locate_board_config(device_abs, board_config_arg)
    if not board_config.is_absolute():
        board_config = source_root / board_config
    variables = parse_board_config(board_config, source_root)
    block, blockers, warnings = build_module(device_rel, variables, source_root)
    inferred_version = ""
    if not variables.get("TARGET_KERNEL_VERSION"):
        inferred_version = infer_kernel_version(source_root, variables.get("TARGET_KERNEL_SOURCE", ""))
        if inferred_version:
            warnings.append(f"TARGET_KERNEL_VERSION will be set to {inferred_version}; the legacy auto-detection is skipped in Soong mode")
        else:
            blockers.append("TARGET_KERNEL_VERSION is missing and could not be inferred from the kernel Makefile")
    product_mk = device_abs / "common.mk"
    if not product_mk.is_file():
        product_mk = device_abs / "device.mk"
    if not product_mk.is_file():
        blockers.append("no common.mk or device.mk was found for PRODUCT_PACKAGES")
    return Migration(device_rel, board_config, device_abs / "Android.bp", product_mk, variables, block, inferred_version, blockers, warnings)


def remove_assignments(text: str, names: set[str]) -> str:
    output: list[str] = []
    skipping = False
    for line in text.splitlines(keepends=True):
        if not skipping:
            match = re.match(r"^\s*([A-Za-z0-9_]+)\s*(?::=|\?=|\+=|=)", line)
            if match and match.group(1) in names:
                skipping = line.rstrip().endswith("\\")
                continue
            output.append(line)
        else:
            skipping = line.rstrip().endswith("\\")
    return "".join(output)


def apply(migration: Migration) -> None:
    if migration.blockers:
        raise ValueError("migration has blockers; apply is disabled")
    android_bp = migration.android_bp.read_text() if migration.android_bp.exists() else ""
    if re.search(r"\buwu_kernel\s*\{", android_bp):
        raise ValueError(f"{migration.android_bp} already declares uwu_kernel")
    board = migration.board_config.read_text()
    board = remove_assignments(board, REMOVED_VARIABLES)
    marker = "# Kernel"
    settings = f"BOARD_USES_SOONG_KERNEL := true\nSOONG_KERNEL_MODULE := {migration.module_label}\n"
    if migration.inferred_kernel_version:
        settings += f"TARGET_KERNEL_VERSION := {migration.inferred_kernel_version}\n"
    if "BOARD_USES_SOONG_KERNEL" not in board:
        if marker in board:
            board = board.replace(marker, marker + "\n" + settings, 1)
        else:
            board = board.rstrip() + "\n\n# Kernel\n" + settings
    if not migration.product_mk.is_file():
        raise ValueError("no common.mk or device.mk found")
    product = migration.product_mk.read_text()
    if not re.search(r"^\s*kernel\s*(?:\\)?\s*$", product, re.MULTILINE):
        product = product.rstrip() + "\n\n# Kernel\nPRODUCT_PACKAGES += \\\n"
        product += "    kernel\n"
    migration.android_bp.write_text(android_bp.rstrip() + "\n\n" + migration.block)
    migration.board_config.write_text(board)
    migration.product_mk.write_text(product)


def print_text(migration: Migration) -> None:
    print(f"Device:      {migration.device_dir}")
    print(f"BoardConfig: {migration.board_config}")
    print("\nGenerated Android.bp block:\n")
    print(migration.block, end="")
    if migration.blockers:
        print("\nBlockers:")
        for blocker in migration.blockers:
            print(f"  - {blocker}")
    if migration.warnings:
        print("\nWarnings:")
        for warning in migration.warnings:
            print(f"  - {warning}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("plan", "apply"), nargs="?", default="plan")
    parser.add_argument("--source-root", default=str(Path(__file__).resolve().parent.parent))
    device = parser.add_mutually_exclusive_group(required=True)
    device.add_argument("--device")
    device.add_argument("--device-dir")
    parser.add_argument("--board-config")
    parser.add_argument("--format", choices=("text", "json"), default="text")
    args = parser.parse_args()
    try:
        migration = analyze(Path(args.source_root).resolve(), args.device_dir or args.device, args.board_config)
        if args.format == "json":
            print(json.dumps({
                "device": migration.device_dir.as_posix(),
                "board_config": str(migration.board_config),
                "module": migration.block,
                "blockers": migration.blockers,
                "warnings": migration.warnings,
                "inferred_kernel_version": migration.inferred_kernel_version,
                "applicable": not migration.blockers,
            }, indent=2))
        else:
            print_text(migration)
        if args.action == "apply":
            apply(migration)
            if args.format == "text":
                print("\nMigration applied.")
        return 2 if migration.blockers else 0
    except (OSError, ValueError) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
