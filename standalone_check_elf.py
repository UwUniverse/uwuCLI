#!/usr/bin/env python3


# Copyright (C) 2026 The uwuAOSP Project
# SPDX-License-Identifier: Apache-2.0

# A script to check extracted proprietary ELF files without running Soong or Ninja.

from __future__ import annotations

import argparse
import ast
import concurrent.futures
import functools
import hashlib
import json
import os
import re
import struct
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path


ELF_MAGIC = b"\x7fELF"
SOURCE_TREES = ("frameworks", "hardware", "vendor", "bionic")
SYSTEM_SHARED_LIBS = ("libc", "libm", "libdl")
INCREMENTAL_VERSION = 2
USE_COLOR = (
    sys.stdout.isatty()
    and os.environ.get("TERM", "dumb") != "dumb"
    and os.environ.get("NO_COLOR") is None
    and os.environ.get("UWU_NO_COLOR") != "true"
)
USE_TRUECOLOR = USE_COLOR and (
    os.environ.get("COLORTERM") in {"truecolor", "24bit"}
    or "direct" in os.environ.get("TERM", "")
)
BOLD = "\033[1m" if USE_COLOR else ""
DIM = "\033[2m" if USE_COLOR else ""
GREEN = "\033[32m" if USE_COLOR else ""
RED = "\033[31m" if USE_COLOR else ""
CYAN = "\033[36m" if USE_COLOR else ""
RESET = "\033[0m" if USE_COLOR else ""
CHECKED_MODULE_TYPES = {"cc_prebuilt_library_shared", "cc_prebuilt_binary"}
SOURCE_MODULE_TYPES = {
    "cc_library",
    "cc_library_shared",
    "cc_prebuilt_library",
    "cc_prebuilt_library_shared",
    "cc_binary",
    "cc_binary_host",
    "cc_prebuilt_binary",
    "aidl_interface",
    "hidl_interface",
    "ndk_library",
}
TOKEN_RE = re.compile(
    r'\s+|//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\])*"|[A-Za-z_][A-Za-z0-9_.-]*|\+=|[{}\[\]:,=+]|.',
    re.DOTALL,
)


def print_banner(top: Path) -> None:
    print()
    if USE_TRUECOLOR:
        colors = (
            "\033[38;2;141;227;253m",
            "\033[38;2;146;216;252m",
            "\033[38;2;151;206;251m",
            "\033[38;2;155;195;251m",
            "\033[38;2;166;203;252m",
            "\033[38;2;177;211;252m",
            "\033[38;2;188;219;253m",
        )
        brand = "".join(f"{color}{BOLD}{char}" for color, char in zip(colors, "uwuAOSP"))
    else:
        brand = f"{CYAN}{BOLD}uwuAOSP"
    print(f"  {brand}{RESET} {BOLD}ELF Checker{RESET}")
    print(f"{DIM}  Check extracted proprietary ELF files without Soong or Ninja{RESET}")
    print(f"{DIM}  source: {top.name}{RESET}")
    print()


def print_report(
    total: int,
    checked: int,
    skipped: int,
    failures: int,
    required_modules: set[str],
) -> None:
    failed = failures > 0
    result = f"{RED}{BOLD}FAILED{RESET}" if failed else f"{GREEN}{BOLD}PASSED{RESET}"
    print()
    print(f"{BOLD}ELF check report{RESET}")
    print(f"{DIM}----------------{RESET}")
    print(f"  Result          {result}")
    print(f"  Discovered      {total}")
    print(f"  Checked         {checked}")
    print(f"  Skipped         {skipped}")
    print(f"  Failed/blocked  {failures}")
    if required_modules:
        print()
        print(f"{BOLD}Required build{RESET}")
        print(f"{DIM}--------------{RESET}")
        print("  " + "m " + " ".join(sorted(required_modules)))


@dataclass
class PropFile:
    path: str
    args: set[str]
    list_path: Path
    vendor_root: Path


@dataclass
class Module:
    module_type: str
    name: str
    bp_path: Path
    props: dict


@dataclass
class ElfInfo:
    path: Path
    bits: int
    machine: int
    soname: str
    needed: list[str]
    imported: frozenset[str]
    exported: frozenset[str]


@dataclass
class CheckPlan:
    prop: PropFile
    elf: ElfInfo
    module: Module
    dependencies: list[Path] = field(default_factory=list)
    missing_modules: list[str] = field(default_factory=list)
    errors: list[str] = field(default_factory=list)


class BlueprintParser:
    def __init__(self, text: str):
        self.tokens = [
            token
            for token in TOKEN_RE.findall(text)
            if not token.isspace() and not token.startswith(("//", "/*"))
        ]
        self.pos = 0

    def peek(self) -> str | None:
        return self.tokens[self.pos] if self.pos < len(self.tokens) else None

    def take(self) -> str:
        token = self.peek()
        if token is None:
            raise ValueError("unexpected end of Android.bp")
        self.pos += 1
        return token

    def accept(self, token: str) -> bool:
        if self.peek() != token:
            return False
        self.pos += 1
        return True

    def value(self):
        token = self.take()
        if token == "{":
            result = {}
            while not self.accept("}"):
                key = self.take()
                if not self.accept(":"):
                    self.skip_value()
                    continue
                result[key] = self.value()
                self.accept(",")
            return result
        if token == "[":
            result = []
            while not self.accept("]"):
                result.append(self.value())
                self.accept(",")
            return result
        if token.startswith('"'):
            return bytes(token[1:-1], "utf-8").decode("unicode_escape")
        if token == "true":
            return True
        if token == "false":
            return False
        return token

    def skip_value(self):
        depth = 0
        while self.peek() is not None:
            token = self.take()
            if token in ("{", "["):
                depth += 1
            elif token in ("}", "]"):
                if depth == 0:
                    self.pos -= 1
                    return
                depth -= 1
            elif token == "," and depth == 0:
                return

    def modules(self, bp_path: Path) -> list[Module]:
        modules = []
        while self.peek() is not None:
            module_type = self.take()
            if self.peek() in ("=", "+="):
                self.take()
                self.value()
                continue
            if self.peek() != "{":
                continue
            props = self.value()
            name = props.get("name")
            if isinstance(name, str):
                modules.append(Module(module_type, name, bp_path, props))
        return modules


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Check proprietary ELF files without Soong or Ninja."
    )
    parser.add_argument(
        "device_dir",
        type=Path,
        help="device directory containing extract-files.py and AndroidProducts.mk",
    )
    parser.add_argument(
        "--product",
        help="override the product inferred from AndroidProducts.mk",
    )
    parser.add_argument(
        "--fail-fast",
        action="store_true",
        help="stop after the first blob that cannot be checked or fails",
    )
    parser.add_argument(
        "-j",
        "--jobs",
        type=int,
        default=1,
        help="number of parallel checks in normal mode (default: %(default)s)",
    )
    parser.add_argument(
        "--incremental",
        action="store_true",
        help="skip unchanged ELF files whose previous check succeeded",
    )
    parser.add_argument(
        "--no-banner",
        action="store_true",
        help="do not print the uwuAOSP ELF Checker banner",
    )
    return parser.parse_args()


def find_top() -> Path:
    current = Path.cwd().resolve()
    for directory in (current, *current.parents):
        if (directory / "build/make/tools/check_elf_file.py").is_file():
            return directory
    raise SystemExit("error: run this script inside an Android source tree")


def string_arg(call: ast.Call, index: int) -> str | None:
    if index >= len(call.args):
        return None
    value = call.args[index]
    return value.value if isinstance(value, ast.Constant) and isinstance(value.value, str) else None


def discover_file_lists(top: Path, device_dir: Path) -> list[Path]:
    extract_path = device_dir / "extract-files.py"
    if not extract_path.is_file():
        raise ValueError(f"file not found: {extract_path}")
    try:
        tree = ast.parse(extract_path.read_text(encoding="utf-8"), filename=str(extract_path))
    except (OSError, UnicodeError, SyntaxError) as error:
        raise ValueError(f"failed to parse {extract_path}: {error}") from error

    module_vendor = None
    common = None
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        function = node.func
        function_name = function.attr if isinstance(function, ast.Attribute) else getattr(function, "id", None)
        if function_name == "ExtractUtilsModule":
            module_vendor = string_arg(node, 1)
        elif function_name == "device_with_common":
            common = string_arg(node, 1)

    device_list = device_dir / "proprietary-files.txt"
    lists = [device_list]
    if common:
        if not module_vendor:
            try:
                relative = device_dir.relative_to(top / "device")
                module_vendor = relative.parts[0]
            except (ValueError, IndexError) as error:
                raise ValueError(f"cannot infer vendor for common device {common}") from error
        lists.append(top / "device" / module_vendor / common / "proprietary-files.txt")
    for path in lists:
        if not path.is_file():
            raise ValueError(f"file not found: {path}")
    return lists


def infer_product(top: Path, device_dir: Path, override: str | None) -> str:
    if override:
        return override
    products_path = device_dir / "AndroidProducts.mk"
    if not products_path.is_file():
        raise ValueError(f"file not found: {products_path}")
    try:
        text = products_path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise ValueError(f"failed to read {products_path}: {error}") from error

    entries = re.findall(
        r"(?:(?P<alias>[A-Za-z0-9_.-]+):)?\$\(LOCAL_DIR\)/(?P<path>[^\s\\]+\.mk)",
        text,
    )
    products = []
    for alias, relative_path in entries:
        makefile = device_dir / relative_path
        product = None
        product_device = None
        if makefile.is_file():
            make_text = makefile.read_text(encoding="utf-8")
            name_match = re.search(r"^\s*PRODUCT_NAME\s*:?=\s*([^\s#]+)", make_text, re.MULTILINE)
            device_match = re.search(r"^\s*PRODUCT_DEVICE\s*:?=\s*([^\s#]+)", make_text, re.MULTILINE)
            if device_match:
                product_device = device_match.group(1)
            elif name_match:
                product = name_match.group(1)
        if product_device:
            products.append(product_device)
        elif product or alias:
            products.append(product or alias)

    products = list(dict.fromkeys(products))
    if len(products) == 1:
        return products[0]
    detail = ", ".join(products) if products else "none"
    raise ValueError(
        f"cannot uniquely infer product from {products_path} (candidates: {detail}); "
        "specify --product"
    )


def infer_vendor_root(top: Path, list_path: Path) -> Path:
    resolved = list_path.resolve()
    try:
        relative = resolved.relative_to(top)
    except ValueError as error:
        raise ValueError(f"{list_path} is outside the Android source tree") from error
    parts = relative.parts
    if len(parts) < 4 or parts[0] != "device":
        raise ValueError(f"cannot infer vendor path from {relative}")
    return top / "vendor" / parts[1] / parts[2]


def parse_prop_file(top: Path, list_path: Path) -> list[PropFile]:
    vendor_root = infer_vendor_root(top, list_path)
    files = []
    for raw_line in list_path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("-"):
            line = line[1:]
        base = re.split(r"[;|]", line, maxsplit=1)[0]
        source_dest = base.split(":", maxsplit=1)
        destination = source_dest[-1]
        args = {
            part.split("=", maxsplit=1)[0]
            for part in line.split(";")[1:]
            if part
        }
        if "DISABLE_CHECKELF" in args or "DISABLE_DEPS" in args:
            continue
        files.append(PropFile(destination, args, list_path, vendor_root))
    return files


def parse_bp(bp_path: Path) -> list[Module]:
    try:
        return BlueprintParser(bp_path.read_text(encoding="utf-8")).modules(bp_path)
    except (OSError, UnicodeError, ValueError) as error:
        raise ValueError(f"failed to parse {bp_path}: {error}") from error


def string_list(value) -> list[str]:
    if not isinstance(value, list):
        return []
    return [item for item in value if isinstance(item, str)]


def module_values(module: Module, key: str, bits: int) -> list[str]:
    values = string_list(module.props.get(key))
    target = module.props.get("target")
    if isinstance(target, dict):
        arch = target.get("android_arm64" if bits == 64 else "android_arm")
        if isinstance(arch, dict):
            values.extend(string_list(arch.get(key)))
    arch_props = module.props.get("arch")
    if isinstance(arch_props, dict):
        arch = arch_props.get("arm64" if bits == 64 else "arm")
        if isinstance(arch, dict):
            values.extend(string_list(arch.get(key)))
    return values


@functools.lru_cache(maxsize=None)
def read_elf(path: Path, llvm_readobj: Path) -> ElfInfo | None:
    try:
        with path.open("rb") as elf_file:
            header = elf_file.read(20)
    except OSError:
        return None
    if len(header) < 20 or header[:4] != ELF_MAGIC:
        return None
    bits = {1: 32, 2: 64}.get(header[4])
    if bits is None:
        return None
    endian = "<" if header[5] == 1 else ">"
    machine = struct.unpack_from(endian + "H", header, 18)[0]
    command = [str(llvm_readobj), "--dynamic-table", "--dyn-symbols", str(path)]
    result = subprocess.run(command, text=True, capture_output=True, check=False)
    if result.returncode:
        raise ValueError(result.stderr.strip() or f"llvm-readobj failed for {path}")
    needed = re.findall(r"\bNEEDED\s+Shared library: \[(.*?)\]", result.stdout)
    sonames = re.findall(r"\bSONAME\s+Library soname: \[(.*?)\]", result.stdout)
    imported = set()
    exported = set()
    for block in re.findall(r"\s+Symbol \{(.*?)\n\s+\}", result.stdout, re.DOTALL):
        name_match = re.search(r"^\s*Name: (.*?)(?: \(\d+\))?$", block, re.MULTILINE)
        section_match = re.search(r"^\s*Section: (.*?) \(", block, re.MULTILINE)
        binding_match = re.search(r"^\s*Binding: (\w+)", block, re.MULTILINE)
        if not name_match or not section_match or not binding_match:
            continue
        name = name_match.group(1).replace("@@", "@")
        if not name:
            continue
        if section_match.group(1) == "Undefined":
            if binding_match.group(1) != "Weak":
                imported.add(name)
        elif binding_match.group(1) != "Local":
            exported.add(name)
            exported.add(name.split("@", maxsplit=1)[0])
    return ElfInfo(
        path,
        bits,
        machine,
        sonames[0] if sonames else path.name,
        needed,
        frozenset(imported),
        frozenset(exported),
    )


def find_llvm_readobj(top: Path) -> Path:
    clang_root = top / "prebuilts/clang/host/linux-x86"
    candidates = sorted(clang_root.glob("clang-r*/bin/llvm-readobj"))
    if not candidates:
        raise SystemExit("error: cannot find a prebuilt llvm-readobj")
    return candidates[-1]


def build_prebuilt_modules(props: list[PropFile]) -> tuple[dict[str, Module], list[str]]:
    modules_by_src = {}
    errors = []
    for vendor_root in sorted({prop.vendor_root for prop in props}):
        bp_path = vendor_root / "Android.bp"
        if not bp_path.is_file():
            errors.append(f"missing generated file: {bp_path}")
            continue
        try:
            modules = parse_bp(bp_path)
        except ValueError as error:
            errors.append(str(error))
            continue
        for module in modules:
            if module.module_type not in CHECKED_MODULE_TYPES:
                continue
            for bits in (32, 64):
                for src in module_values(module, "srcs", bits):
                    source = (bp_path.parent / src).resolve()
                    modules_by_src[str(source)] = module
    return modules_by_src, errors


def modules_by_name(modules_by_src: dict[str, Module]) -> dict[str, list[tuple[Module, Path]]]:
    result: dict[str, list[tuple[Module, Path]]] = {}
    for source, module in modules_by_src.items():
        entry = (module, Path(source))
        if entry not in result.setdefault(module.name, []):
            result[module.name].append(entry)
    return result


def collect_source_modules(top: Path, wanted: set[str]) -> dict[str, list[Module]]:
    found: dict[str, list[Module]] = {}
    if not wanted:
        return found
    aliases = {name: name for name in wanted}
    for name in wanted:
        match = re.fullmatch(r"(.+)-V\d+-ndk(?:-platform)?", name)
        if match:
            aliases[match.group(1)] = name
    name_pattern = re.compile(
        r'\bname\s*:\s*"('
        + "|".join(re.escape(name) for name in sorted(aliases))
        + r')"'
    )
    for tree in SOURCE_TREES:
        root = top / tree
        if not root.is_dir():
            continue
        for bp_path in root.rglob("Android.bp"):
            try:
                text = bp_path.read_text(encoding="utf-8")
            except (OSError, UnicodeError):
                continue
            if not name_pattern.search(text):
                continue
            try:
                modules = parse_bp(bp_path)
            except ValueError:
                continue
            for module in modules:
                dependency_name = aliases.get(module.name)
                if dependency_name and module.module_type in SOURCE_MODULE_TYPES:
                    if dependency_name != module.name:
                        module = Module(
                            module.module_type,
                            dependency_name,
                            module.bp_path,
                            module.props,
                        )
                    found.setdefault(dependency_name, []).append(module)
    return found


def build_product_index(top: Path, product: str) -> dict[str, list[Path]]:
    index: dict[str, list[Path]] = {}
    product_out = top / "out/target/product" / product
    if not product_out.is_dir():
        return index
    for root, _, files in os.walk(product_out):
        root_path = Path(root)
        for filename in files:
            index.setdefault(filename, []).append(root_path / filename)
    return index


def output_candidates(
    product_index: dict[str, list[Path]], module: Module, bits: int
) -> list[Path]:
    libdir = "lib64" if bits == 64 else "lib"
    stem = module.props.get("stem")
    filename = stem if isinstance(stem, str) else module.name
    if module.module_type not in ("cc_binary", "cc_binary_host", "cc_prebuilt_binary"):
        if not filename.endswith(".so"):
            filename += ".so"
    return [
        path
        for path in product_index.get(filename, [])
        if libdir in path.parts and path.is_file()
    ]


@functools.lru_cache(maxsize=None)
def platform_prebuilt_candidates(top: Path, soname: str, bits: int) -> tuple[Path, ...]:
    arch = "arm64" if bits == 64 else "arm"
    candidates = list(
        (top / "prebuilts/runtime/mainline/runtime/sdk/android" / arch).glob(
            f"**/{soname}"
        )
    )
    vndk_root = top / "prebuilts/vndk"
    versions = sorted(
        (
            path
            for path in vndk_root.glob("v*")
            if path.name[1:].isdigit()
        ),
        key=lambda path: int(path.name[1:]),
        reverse=True,
    )
    for version in versions:
        candidates.extend((version / arch).glob(f"**/{soname}"))
    return tuple(path for path in candidates if path.is_file())


def llndk_prebuilt_candidates(top: Path, soname: str, bits: int) -> tuple[Path, ...]:
    return tuple(
        path
        for path in platform_prebuilt_candidates(top, soname, bits)
        if "llndk-stub" in path.parts
    )


def pick_matching_elf(
    paths: list[Path] | tuple[Path, ...],
    expected_soname: str,
    target: ElfInfo,
    llvm_readobj: Path,
) -> Path | None:
    matches = []
    for path in paths:
        try:
            info = read_elf(path, llvm_readobj)
        except ValueError:
            continue
        if (
            info is not None
            and info.bits == target.bits
            and info.machine == target.machine
            and info.soname == expected_soname
        ):
            matches.append(info)
    if not matches:
        return None
    return max(matches, key=lambda info: len(target.imported & info.exported)).path


def source_prop_candidates(props: list[PropFile], soname: str) -> list[Path]:
    result = []
    for prop in props:
        if Path(prop.path).name == soname:
            result.append(prop.vendor_root / "proprietary" / prop.path)
    return result


def make_plan(
    top: Path,
    product_index: dict[str, list[Path]],
    prop: PropFile,
    elf: ElfInfo,
    module: Module,
    all_props: list[PropFile],
    prebuilt_by_name: dict[str, list[tuple[Module, Path]]],
    source_modules: dict[str, list[Module]],
    llvm_readobj: Path,
) -> CheckPlan:
    plan = CheckPlan(prop, elf, module)
    declared = module_values(module, "shared_libs", elf.bits)
    declared.extend(module_values(module, "system_shared_libs", elf.bits))
    if not declared:
        declared = list(SYSTEM_SHARED_LIBS)

    declared_by_soname: dict[str, tuple[str, list[Path], list[Module]]] = {}
    for dependency in declared:
        prebuilt_entries = prebuilt_by_name.get(dependency, [])
        source_entries = source_modules.get(dependency, [])
        dependency_module = (
            prebuilt_entries[0][0]
            if prebuilt_entries
            else source_entries[0]
            if source_entries
            else None
        )
        stem = dependency_module.props.get("stem") if dependency_module else None
        filename = stem if isinstance(stem, str) else dependency
        soname = filename if filename.endswith(".so") else filename + ".so"
        source_paths = [path for _, path in prebuilt_entries]
        declared_by_soname.setdefault(soname, (dependency, source_paths, source_entries))
        if dependency.startswith("libclang_rt."):
            arch = "aarch64" if elf.bits == 64 else "arm"
            runtime_soname = f"{dependency}-{arch}-android.so"
            declared_by_soname.setdefault(
                runtime_soname, (dependency, source_paths, source_entries)
            )

    for soname in elf.needed:
        declaration = declared_by_soname.get(soname)
        if declaration is None:
            plan.errors.append(f'DT_NEEDED "{soname}" is not declared in shared_libs')
            continue
        dependency_name, prebuilt_paths, source_entries = declaration
        source_paths = prebuilt_paths or source_prop_candidates(all_props, soname)
        dependency_path = pick_matching_elf(source_paths, soname, elf, llvm_readobj)
        if dependency_path is not None:
            plan.dependencies.append(dependency_path)
            continue

        output_paths = []
        for source_module in source_entries:
            output_paths.extend(output_candidates(product_index, source_module, elf.bits))
        output_paths.extend(llndk_prebuilt_candidates(top, soname, elf.bits))
        dependency_path = pick_matching_elf(output_paths, soname, elf, llvm_readobj)
        if dependency_path is not None:
            plan.dependencies.append(dependency_path)
            continue

        libdir = "lib64" if elf.bits == 64 else "lib"
        installed = [
            path
            for path in product_index.get(soname, [])
            if libdir in path.parts and path.is_file()
        ]
        installed.extend(llndk_prebuilt_candidates(top, soname, elf.bits))
        dependency_path = pick_matching_elf(installed, soname, elf, llvm_readobj)
        if dependency_path is not None:
            plan.dependencies.append(dependency_path)
            continue

        dependency_path = pick_matching_elf(
            platform_prebuilt_candidates(top, soname, elf.bits),
            soname,
            elf,
            llvm_readobj,
        )
        if dependency_path is not None:
            plan.dependencies.append(dependency_path)
            continue

        if source_entries:
            plan.missing_modules.append(dependency_name)
        elif prebuilt_paths:
            plan.errors.append(
                f'cannot read extracted dependency "{dependency_name}" ({soname})'
            )
        else:
            plan.errors.append(
                f'cannot find "{dependency_name}" ({soname}) in product outputs or '
                f"Android.bp files under {', '.join(SOURCE_TREES)}"
            )

    plan.missing_modules = sorted(set(plan.missing_modules))
    return plan


def expected_soname(plan: CheckPlan) -> str | None:
    if plan.module.module_type == "cc_prebuilt_library_shared":
        stem = plan.module.props.get("stem")
        soname = str(stem or plan.module.name)
        return soname if soname.endswith(".so") else soname + ".so"
    return None


def check_plan(plan: CheckPlan, llvm_readobj: Path) -> list[str]:
    errors = []
    soname = expected_soname(plan)
    if soname and plan.elf.soname != soname:
        errors.append(
            f'DT_SONAME "{plan.elf.soname}" must be equal to the file name "{soname}"'
        )

    dependencies = []
    for path in plan.dependencies:
        try:
            dependency = read_elf(path, llvm_readobj)
        except ValueError as error:
            errors.append(str(error))
            continue
        if dependency is None:
            errors.append(f'dependency is not an ELF file: "{path}"')
        else:
            dependencies.append(dependency)

    available_sonames = {dependency.soname for dependency in dependencies}
    for needed in plan.elf.needed:
        if needed not in available_sonames:
            errors.append(f'DT_NEEDED "{needed}" is not available from resolved dependencies')

    if plan.module.props.get("allow_undefined_symbols") is not True:
        exported = set(plan.elf.exported)
        for dependency in dependencies:
            exported.update(dependency.exported)
        for symbol in sorted(plan.elf.imported - exported):
            errors.append(f"Unresolved symbol: {symbol}")
    return errors


@functools.lru_cache(maxsize=None)
def file_digest(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def plan_state(top: Path, plan: CheckPlan) -> dict:
    def describe(path: Path) -> dict:
        try:
            display = str(path.relative_to(top))
        except ValueError:
            display = str(path)
        stat = path.stat()
        return {
            "path": display,
            "size": stat.st_size,
            "mtime_ns": stat.st_mtime_ns,
            "sha256": file_digest(path),
        }

    return {
        "target": describe(plan.elf.path),
        "dependencies": [describe(path) for path in sorted(set(plan.dependencies))],
        "module": {
            "type": plan.module.module_type,
            "name": plan.module.name,
            "props": plan.module.props,
        },
    }


def incremental_unchanged(
    top: Path, target: Path, module: Module, previous: dict | None
) -> bool:
    if not previous or previous.get("success") is not True:
        return False
    state = previous.get("state")
    if not isinstance(state, dict):
        return False
    expected_module = {
        "type": module.module_type,
        "name": module.name,
        "props": module.props,
    }
    if state.get("module") != expected_module:
        return False

    files = [state.get("target"), *state.get("dependencies", [])]
    if not all(isinstance(item, dict) for item in files):
        return False
    for item in files:
        path = Path(item["path"])
        if not path.is_absolute():
            path = top / path
        try:
            stat = path.stat()
        except OSError:
            return False
        if stat.st_size != item.get("size") or stat.st_mtime_ns != item.get("mtime_ns"):
            return False
    try:
        return target.resolve() == (
            top / state["target"]["path"]
            if not Path(state["target"]["path"]).is_absolute()
            else Path(state["target"]["path"])
        ).resolve()
    except (KeyError, TypeError):
        return False


def load_incremental(path: Path) -> dict:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        return {}
    if data.get("version") != INCREMENTAL_VERSION:
        return {}
    files = data.get("files")
    return files if isinstance(files, dict) else {}


def save_incremental(path: Path, files: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(
            {"version": INCREMENTAL_VERSION, "files": files},
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    temporary.replace(path)


def run_check(
    top: Path,
    product_index: dict[str, list[Path]],
    target: tuple[PropFile, ElfInfo, Module],
    all_props: list[PropFile],
    prebuilt_by_name: dict[str, list[tuple[Module, Path]]],
    source_modules: dict[str, list[Module]],
    llvm_readobj: Path,
    previous: dict | None,
) -> tuple[bool, bool, bool, str, CheckPlan, dict | None]:
    prop, elf, module = target
    plan = make_plan(
        top,
        product_index,
        prop,
        elf,
        module,
        all_props,
        prebuilt_by_name,
        source_modules,
        llvm_readobj,
    )
    if plan.missing_modules:
        return (
            False,
            False,
            False,
            f"{plan.elf.path}: blocked by missing build output for "
            f"{', '.join(plan.missing_modules)}",
            plan,
            None,
        )
    if plan.errors:
        return (
            False,
            False,
            False,
            "\n".join(f"{plan.elf.path}: error: {error}" for error in plan.errors),
            plan,
            None,
        )
    try:
        state = plan_state(top, plan)
    except OSError as error:
        return False, False, False, f"{plan.elf.path}: error: {error}", plan, None
    if previous and previous.get("success") is True and previous.get("state") == state:
        return (
            True,
            False,
            True,
            f"Skipped unchanged elf file: {plan.prop.path}",
            plan,
            state,
        )
    errors = check_plan(plan, llvm_readobj)
    if errors:
        output = "\n".join(f"{plan.elf.path}: error: {error}" for error in errors)
        return False, True, False, output, plan, state
    return (
        True,
        True,
        False,
        f"{GREEN}Successfully checked elf file: {plan.prop.path}{RESET}",
        plan,
        state,
    )


def main() -> int:
    args = parse_args()
    if args.jobs < 1:
        print("error: --jobs must be at least 1", file=sys.stderr)
        return 2
    top = find_top()
    if not args.no_banner:
        print_banner(top)
    device_dir = (
        args.device_dir if args.device_dir.is_absolute() else top / args.device_dir
    ).resolve()
    try:
        device_dir.relative_to(top / "device")
        list_paths = discover_file_lists(top, device_dir)
        product = infer_product(top, device_dir, args.product)
    except ValueError as error:
        print(f"error: {error}", file=sys.stderr)
        return 2
    print(f"[info] Product output: out/target/product/{product}", flush=True)
    for list_path in list_paths:
        print(f"[info] File list: {list_path.relative_to(top)}", flush=True)

    llvm_readobj = find_llvm_readobj(top)
    all_props = []
    try:
        for list_path in list_paths:
            all_props.extend(parse_prop_file(top, list_path))
    except (OSError, UnicodeError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 2

    print("[info] Parsing generated proprietary Android.bp files...", flush=True)
    prebuilt_modules, bp_errors = build_prebuilt_modules(all_props)
    prebuilt_by_name = modules_by_name(prebuilt_modules)
    for error in bp_errors:
        print(f"error: {error}", file=sys.stderr)
    if bp_errors:
        return 2

    incremental_path = top / "out/standalone_check_elf" / f"{product}.json"
    previous_files = load_incremental(incremental_path) if args.incremental else {}
    incremental_files = dict(previous_files)
    targets = []
    early_skipped: list[PropFile] = []
    wanted_modules = set()
    failures = 0
    print(
        f"[info] Analyzing {len(prebuilt_modules)} generated ELF module file(s)...",
        flush=True,
    )
    for prop in all_props:
        path = (prop.vendor_root / "proprietary" / prop.path).resolve()
        module = prebuilt_modules.get(str(path))
        # Files emitted through PRODUCT_COPY_FILES do not get Soong's module-level
        # ELF check, even when their contents happen to have an ELF header.
        if module is None:
            continue
        if not path.is_file():
            print(f"error: extracted file not found: {path}", file=sys.stderr)
            failures += 1
            if args.fail_fast:
                return 1
            continue
        key = str(path.relative_to(top))
        if args.incremental and incremental_unchanged(
            top, path, module, previous_files.get(key)
        ):
            early_skipped.append(prop)
            continue
        try:
            elf = read_elf(path, llvm_readobj)
        except ValueError as error:
            print(f"error: {error}", file=sys.stderr)
            failures += 1
            if args.fail_fast:
                return 1
            continue
        if elf is None:
            print(f"error: generated ELF module contains a non-ELF file: {path}", file=sys.stderr)
            failures += 1
            if args.fail_fast:
                return 1
            continue
        if module.props.get("check_elf_files") is False:
            continue
        declared = module_values(module, "shared_libs", elf.bits)
        declared.extend(module_values(module, "system_shared_libs", elf.bits))
        wanted_modules.update(declared)
        targets.append((prop, elf, module))

    source_modules = {}
    product_index = {}
    if targets:
        print(
            f"[info] Searching {', '.join(SOURCE_TREES)} for "
            f"{len(wanted_modules)} dependency module(s)...",
            flush=True,
        )
        source_modules = collect_source_modules(top, wanted_modules)
        print(f"[info] Indexing out/target/product/{product}...", flush=True)
        product_index = build_product_index(top, product)
    if args.incremental:
        print(f"[info] Incremental state: {incremental_path.relative_to(top)}", flush=True)
    jobs = 1 if args.fail_fast else args.jobs
    print(
        f"[info] Checking {len(targets)} ELF file(s) with {jobs} worker(s)...",
        flush=True,
    )

    checked = 0
    skipped_count = len(early_skipped)
    completed = 0
    total = len(early_skipped) + len(targets)
    required_modules: set[str] = set()
    for prop in early_skipped:
        completed += 1
        print(
            f"{BOLD}[{completed}/{total}]{RESET} Skipped unchanged elf file: {prop.path}",
            flush=True,
        )
    if args.fail_fast:
        results = (
            run_check(
                top,
                product_index,
                target,
                all_props,
                prebuilt_by_name,
                source_modules,
                llvm_readobj,
                previous_files.get(str(target[1].path.relative_to(top))),
            )
            for target in targets
        )
        for success, attempted, skipped, output, plan, state in results:
            completed += 1
            print(
                f"{BOLD}[{completed}/{total}]{RESET} {output}",
                file=sys.stdout if success else sys.stderr,
                flush=True,
            )
            checked += int(attempted)
            skipped_count += int(skipped)
            key = str(plan.elf.path.relative_to(top))
            incremental_files[key] = {"success": success, "state": state}
            required_modules.update(plan.missing_modules)
            if args.incremental:
                save_incremental(incremental_path, incremental_files)
            if not success:
                print_report(
                    total,
                    checked,
                    skipped_count,
                    1,
                    required_modules,
                )
                return 1
    else:
        with concurrent.futures.ThreadPoolExecutor(max_workers=jobs) as executor:
            futures = [
                executor.submit(
                    run_check,
                    top,
                    product_index,
                    target,
                    all_props,
                    prebuilt_by_name,
                    source_modules,
                    llvm_readobj,
                    previous_files.get(str(target[1].path.relative_to(top))),
                )
                for target in targets
            ]
            for future in concurrent.futures.as_completed(futures):
                try:
                    success, attempted, skipped, output, plan, state = future.result()
                except Exception as error:
                    success, attempted, skipped = False, False, False
                    output = f"error: {error}"
                    plan = None
                    state = None
                completed += 1
                print(
                    f"{BOLD}[{completed}/{total}]{RESET} {output}",
                    file=sys.stdout if success else sys.stderr,
                    flush=True,
                )
                checked += int(attempted)
                skipped_count += int(skipped)
                failures += int(not success)
                if plan is not None:
                    key = str(plan.elf.path.relative_to(top))
                    incremental_files[key] = {"success": success, "state": state}
                    required_modules.update(plan.missing_modules)

    if args.incremental:
        save_incremental(incremental_path, incremental_files)

    print_report(total, checked, skipped_count, failures, required_modules)
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
