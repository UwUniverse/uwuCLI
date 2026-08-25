#!/usr/bin/env python3

# Copyright (C) 2026 The uwuAOSP Project
# SPDX-License-Identifier: Apache-2.0

"""Analyze product packages that block switching a generated product to Soong-only."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


PRODUCT_PACKAGE_FIELDS = (
    "ProductPackages",
    "ProductPackagesDebug",
    "ProductPackagesEng",
    "ProductPackagesDebugAsan",
    "ProductPackagesDebugJavaCoverage",
    "ProductPackagesArm64",
    "ProductPackagesShippingApiLevel29",
    "ProductPackagesShippingApiLevel33",
    "ProductPackagesShippingApiLevel34",
)

ASSIGNMENT = re.compile(r"^([A-Z0-9_]+)\s*:?=\s*(.*?)\s*$")
MODULE_TYPE_COMMENT = re.compile(r"# type: ([^,]+)")
BLUEPRINT_NAME = re.compile(r'\bname\s*:\s*"([^"]+)"')
MAKE_MODULE = re.compile(r"^\s*LOCAL_MODULE\s*:?=\s*([^\s#]+)")


@dataclass
class PackageSelection:
    name: str
    selected_by: set[str] = field(default_factory=set)
    declared_in: set[str] = field(default_factory=set)
    active: bool = False


@dataclass
class SoongModule:
    name: str
    module_types: set[str] = field(default_factory=set)
    module_classes: set[str] = field(default_factory=set)
    module_paths: set[str] = field(default_factory=set)
    overrides: set[str] = field(default_factory=set)
    variants: int = 0
    has_install: bool = False
    has_required: bool = False
    has_installable_variant: bool = False


@dataclass
class PackageResult:
    name: str
    status: str
    priority: str
    active: bool
    selected_by: list[str]
    declared_in: list[str]
    module_paths: list[str]
    module_types: list[str]
    definition_locations: list[str]
    overridden_by: list[str]
    reason: str

    def to_json(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "status": self.status,
            "priority": self.priority,
            "active": self.active,
            "selected_by": self.selected_by,
            "declared_in": self.declared_in,
            "module_paths": self.module_paths,
            "module_types": self.module_types,
            "definition_locations": self.definition_locations,
            "overridden_by": self.overridden_by,
            "reason": self.reason,
        }


@dataclass
class Analysis:
    product: str
    device: str
    variant: str
    architecture: str
    shipping_api_level: int | None
    inputs: dict[str, str]
    packages: list[PackageResult]
    warnings: list[str]
    all_variants: bool

    @property
    def blockers(self) -> list[PackageResult]:
        return [package for package in self.packages if package.status == "make_only"]

    @property
    def ignored_packages(self) -> list[PackageResult]:
        return [package for package in self.packages if package.status.startswith("ignored")]

    @property
    def ready_packages(self) -> list[PackageResult]:
        return [package for package in self.packages if package.status.startswith("ready")]

    def to_json(self) -> dict[str, Any]:
        return {
            "product": self.product,
            "device": self.device,
            "variant": self.variant,
            "architecture": self.architecture,
            "shipping_api_level": self.shipping_api_level,
            "all_variants": self.all_variants,
            "ready": not self.blockers,
            "inputs": self.inputs,
            "summary": {
                "selected": len(self.packages),
                "soong_backed": len(self.ready_packages),
                "blockers": len(self.blockers),
                "ignored": len(self.ignored_packages),
                "warnings": len(self.warnings),
            },
            "warnings": self.warnings,
            "packages": [package.to_json() for package in self.packages],
        }


def load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text())
    except json.JSONDecodeError as error:
        raise ValueError(f"invalid JSON in {path}: {error}") from error
    if not isinstance(value, dict):
        raise ValueError(f"expected a JSON object in {path}")
    return value


def iter_json_array(path: Path):
    decoder = json.JSONDecoder()
    buffer = ""
    started = False
    eof = False
    with path.open() as source:
        while True:
            if not eof and len(buffer) < 65536:
                chunk = source.read(65536)
                if chunk:
                    buffer += chunk
                else:
                    eof = True
            buffer = buffer.lstrip()
            if not started:
                if not buffer:
                    if eof:
                        raise ValueError(f"empty JSON array in {path}")
                    continue
                if buffer[0] != "[":
                    raise ValueError(f"expected a JSON array in {path}")
                buffer = buffer[1:]
                started = True
                continue
            buffer = buffer.lstrip()
            while buffer.startswith(","):
                buffer = buffer[1:].lstrip()
            if buffer.startswith("]"):
                return
            if not buffer:
                if eof:
                    raise ValueError(f"unterminated JSON array in {path}")
                continue
            try:
                value, offset = decoder.raw_decode(buffer)
            except json.JSONDecodeError as error:
                if eof:
                    raise ValueError(f"invalid JSON in {path}: {error}") from error
                chunk = source.read(65536)
                if chunk:
                    buffer += chunk
                else:
                    eof = True
                continue
            yield value
            buffer = buffer[offset:]


def parse_used_environment(path: Path) -> dict[str, str]:
    if not path.is_file():
        return {}
    result: dict[str, str] = {}
    for line in path.read_text().splitlines():
        if "=" not in line:
            continue
        name, value = line.split("=", 1)
        result[name] = value.strip().strip("'\"")
    return result


def parse_api_level(variables: dict[str, Any], extra_variables: dict[str, Any]) -> int | None:
    for source in (variables, extra_variables):
        value = source.get("ShippingApiLevel") or source.get("BoardShippingApiLevel")
        try:
            return int(value)
        except (TypeError, ValueError):
            pass
    return None


def active_package_fields(
    variables: dict[str, Any],
    extra_variables: dict[str, Any],
    used_environment: dict[str, str],
) -> tuple[set[str], int | None]:
    fields = {"ProductPackages"}
    if variables.get("Debuggable"):
        fields.add("ProductPackagesDebug")
        if variables.get("Eng"):
            fields.add("ProductPackagesEng")
        if "address" in variables.get("SanitizeDevice", []):
            fields.add("ProductPackagesDebugAsan")
        if used_environment.get("EMMA_INSTRUMENT") == "true":
            fields.add("ProductPackagesDebugJavaCoverage")

    if "arm64" in (variables.get("DeviceArch"), variables.get("DeviceSecondaryArch")):
        fields.add("ProductPackagesArm64")

    shipping_api = parse_api_level(variables, extra_variables)
    if shipping_api is not None:
        for level in (29, 33, 34):
            if level >= shipping_api:
                fields.add(f"ProductPackagesShippingApiLevel{level}")
    return fields, shipping_api


def collect_package_selections(
    variables: dict[str, Any],
    extra_variables: dict[str, Any],
    used_environment: dict[str, str],
    all_variants: bool,
) -> tuple[dict[str, PackageSelection], int | None]:
    partition_vars = variables.get("PartitionVarsForSoongMigrationOnlyDoNotUse")
    if not isinstance(partition_vars, dict):
        raise ValueError("product variables do not contain PartitionVarsForSoongMigrationOnlyDoNotUse")
    package_sets = partition_vars.get("ProductPackagesSet")
    if not isinstance(package_sets, dict) or not isinstance(package_sets.get("all"), dict):
        raise ValueError("product variables do not contain ProductPackagesSet[\"all\"]")

    active_fields, shipping_api = active_package_fields(variables, extra_variables, used_environment)
    included_fields = set(PRODUCT_PACKAGE_FIELDS) if all_variants else active_fields
    selections: dict[str, PackageSelection] = {}

    def add_packages(values: dict[str, Any], declared_in: str | None) -> None:
        for field_name in included_fields:
            packages = values.get(field_name, [])
            if not isinstance(packages, list):
                raise ValueError(f"{field_name} must be a list")
            for package_name in packages:
                if not isinstance(package_name, str) or not package_name:
                    continue
                selection = selections.setdefault(package_name, PackageSelection(package_name))
                selection.selected_by.add(field_name)
                selection.active = selection.active or field_name in active_fields
                if declared_in:
                    selection.declared_in.add(declared_in)

    add_packages(package_sets["all"], None)
    for makefile, values in package_sets.items():
        if makefile == "all" or not isinstance(values, dict):
            continue
        add_packages(values, makefile)

    return selections, shipping_api


def parse_module_info(path: Path, candidates: set[str]) -> dict[str, SoongModule]:
    if not path.is_file():
        return {}
    modules: dict[str, SoongModule] = {}

    def add_module(name: str, entry: Any) -> None:
        if name not in candidates or not isinstance(entry, dict):
            return
        module = modules.setdefault(name, SoongModule(name=name))
        module.variants += 1
        module_types = entry.get("module_type", [])
        module_classes = entry.get("class", [])
        module_paths = entry.get("path", [])
        if isinstance(module_types, str):
            module_types = [module_types]
        if isinstance(module_classes, str):
            module_classes = [module_classes]
        if isinstance(module_paths, str):
            module_paths = [module_paths]
        module.module_types.update(str(value) for value in module_types if value)
        module.module_classes.update(str(value) for value in module_classes if value)
        module.module_paths.update(str(value) for value in module_paths if value)
        overrides = entry.get("overrides", [])
        if isinstance(overrides, str):
            overrides = [overrides]
        module.overrides.update(str(value) for value in overrides if value)
        installed = entry.get("installed", [])
        module.has_install = module.has_install or bool(installed)
        module.has_installable_variant = module.has_installable_variant or bool(installed) or not entry.get("uninstallable")
        module.has_required = module.has_required or bool(entry.get("required"))

    with path.open() as source:
        first = ""
        while not first:
            first = source.read(1)
            if not first:
                raise ValueError(f"empty module info file: {path}")
            if first.isspace():
                first = ""
    if first == "{":
        data = load_json(path)
        for name in candidates:
            add_module(name, data.get(name))
    elif first == "[":
        for item in iter_json_array(path):
            if not isinstance(item, dict):
                continue
            for name, entry in item.items():
                add_module(name, entry)
    else:
        raise ValueError(f"expected a JSON object or array in {path}")
    return modules


def parse_soong_android_mk(path: Path, candidates: set[str]) -> dict[str, SoongModule]:
    modules: dict[str, SoongModule] = {}
    current_name: str | None = None
    current_path = ""
    current_type = ""
    current_class = ""
    current_install = False
    current_required = False
    current_uninstallable = False
    current_overrides: set[str] = set()

    def commit() -> None:
        nonlocal current_name, current_path, current_type, current_class
        nonlocal current_install, current_required, current_uninstallable
        nonlocal current_overrides
        if current_name in candidates:
            module = modules.setdefault(current_name, SoongModule(current_name))
            module.variants += 1
            module.has_install = module.has_install or current_install
            module.has_required = module.has_required or current_required
            module.has_installable_variant = module.has_installable_variant or not current_uninstallable
            module.overrides.update(current_overrides)
            if current_path:
                module.module_paths.add(current_path)
            if current_type:
                module.module_types.add(current_type)
            if current_class:
                module.module_classes.add(current_class)
        current_name = None
        current_path = ""
        current_type = ""
        current_class = ""
        current_install = False
        current_required = False
        current_uninstallable = False
        current_overrides = set()

    with path.open(errors="replace") as android_mk:
        for raw_line in android_mk:
            line = raw_line.rstrip("\n")
            if line.startswith("include $(CLEAR_VARS)"):
                commit()
                type_comment = MODULE_TYPE_COMMENT.search(line)
                if type_comment:
                    current_type = type_comment.group(1)
                continue
            if current_name in candidates and line.startswith("include $(BUILD_PHONY_PACKAGE)"):
                current_type = "phony"
                continue
            match = ASSIGNMENT.match(line)
            if not match:
                continue
            name, value = match.groups()
            if name == "LOCAL_MODULE":
                current_name = value
                continue
            if name == "LOCAL_PATH":
                current_path = value
                continue
            if current_name not in candidates:
                continue
            if name == "LOCAL_SOONG_MODULE_TYPE":
                current_type = value
            elif name == "LOCAL_MODULE_CLASS":
                current_class = value
            elif name in (
                "LOCAL_MODULE_PATH",
                "LOCAL_SOONG_INSTALLED_MODULE",
                "LOCAL_SOONG_INSTALL_PAIRS",
                "LOCAL_SOONG_INSTALL_SYMLINKS",
            ) and value:
                current_install = True
            elif name in ("LOCAL_REQUIRED_MODULES", "LOCAL_HOST_REQUIRED_MODULES", "LOCAL_TARGET_REQUIRED_MODULES"):
                current_required = True
            elif name == "LOCAL_OVERRIDES_PACKAGES":
                current_overrides.update(value.split())
            elif name == "LOCAL_UNINSTALLABLE_MODULE" and value == "true":
                current_uninstallable = True
    commit()
    return modules


def parse_late_soong_modules(path: Path, candidates: set[str]) -> dict[str, SoongModule]:
    if not candidates:
        return {}
    exact_targets = {f"{name}-soong": name for name in candidates}
    replacement_targets = {f"{name}-soong-soong": name for name in candidates}
    modules: dict[str, SoongModule] = {}
    with path.open(errors="replace") as late_mk:
        for raw_line in late_mk:
            if not raw_line.startswith(".PHONY: "):
                continue
            target = raw_line[len(".PHONY: "):].strip()
            name = exact_targets.get(target)
            module_type = "hidden_soong"
            if name is None:
                name = replacement_targets.get(target)
                module_type = "soong_replacement"
            if name is None or name in modules:
                continue
            modules[name] = SoongModule(
                name=name,
                module_types={module_type},
                variants=1,
                has_required=True,
                has_installable_variant=True,
            )
            if len(modules) == len(candidates):
                break
    return modules


def merge_soong_modules(destination: dict[str, SoongModule], source: dict[str, SoongModule]) -> None:
    for name, incoming in source.items():
        module = destination.setdefault(name, SoongModule(name=name))
        module.module_types.update(incoming.module_types)
        module.module_classes.update(incoming.module_classes)
        module.module_paths.update(incoming.module_paths)
        module.overrides.update(incoming.overrides)
        module.variants += incoming.variants
        module.has_install = module.has_install or incoming.has_install
        module.has_required = module.has_required or incoming.has_required
        module.has_installable_variant = module.has_installable_variant or incoming.has_installable_variant


def parse_make_module_paths(path: Path, candidates: set[str]) -> dict[str, list[str]]:
    if not path.is_file():
        return {}
    data = load_json(path)
    result: dict[str, list[str]] = {}
    for name in candidates:
        entry = data.get(name)
        if not isinstance(entry, dict) or str(entry.get("make", "")).lower() != "true":
            continue
        paths = entry.get("path", [])
        if isinstance(paths, list):
            result[name] = sorted(str(value) for value in paths if value)
    return result


def find_definitions_in_file(path: Path, names: set[str], source_root: Path) -> dict[str, list[str]]:
    if not names or not path.is_file():
        return {}
    result: dict[str, list[str]] = {}
    is_blueprint = path.name == "Android.bp"
    with path.open(errors="replace") as source:
        for line_number, line in enumerate(source, 1):
            if is_blueprint:
                matches = (match.group(1) for match in BLUEPRINT_NAME.finditer(line))
            else:
                match = MAKE_MODULE.match(line)
                matches = (match.group(1),) if match else ()
            for name in matches:
                if name in names:
                    location = f"{display_path(path, source_root)}:{line_number}"
                    result.setdefault(name, []).append(location)
    return result


def find_source_definitions(
    source_root: Path,
    out_dir: Path,
    names: set[str],
    path_hints: dict[str, set[str]],
) -> dict[str, list[str]]:
    result: dict[str, list[str]] = {}

    def merge(found: dict[str, list[str]]) -> None:
        for name, locations in found.items():
            result.setdefault(name, []).extend(location for location in locations if location not in result.get(name, []))

    for name, directories in path_hints.items():
        if name not in names:
            continue
        for directory in directories:
            for filename in ("Android.bp", "Android.mk"):
                merge(find_definitions_in_file(source_root / directory / filename, {name}, source_root))
        if name in result:
            continue

    unresolved = names.difference(result)
    for list_name, filename in (("Android.bp.list", "Android.bp"), ("Android.mk.list", "Android.mk")):
        if not unresolved:
            break
        module_list = out_dir / ".module_paths" / list_name
        if not module_list.is_file():
            continue
        for entry in module_list.read_text().splitlines():
            relative = Path(entry.strip())
            if not entry.strip() or relative.is_absolute() or ".." in relative.parts or relative.name != filename:
                continue
            found = find_definitions_in_file(source_root / relative, unresolved, source_root)
            merge(found)
            unresolved.difference_update(found)
            if not unresolved:
                break
    return {name: sorted(locations) for name, locations in result.items()}


def priority_for(paths: list[str], declared_in: list[str], device: str) -> str:
    sources = paths + declared_in
    if any(path.startswith("device/") and (f"/{device}/" in f"/{path}/" or path.endswith(f"/{device}")) for path in sources):
        return "P0"
    if any(path.startswith(("device/", "vendor/")) for path in sources):
        return "P1"
    return "P2"


def collect_override_providers(
    selections: dict[str, PackageSelection],
    soong_modules: dict[str, SoongModule],
) -> dict[str, list[SoongModule]]:
    overridden_by: dict[str, list[SoongModule]] = {}
    for provider_name in selections:
        provider = soong_modules.get(provider_name)
        if provider is None:
            continue
        effective = (
            provider.has_install
            or provider.has_required
            or "phony" in provider.module_types
            or "hidden_soong" in provider.module_types
            or "soong_replacement" in provider.module_types
        )
        if not effective:
            continue
        for overridden in provider.overrides:
            if overridden in selections:
                overridden_by.setdefault(overridden, []).append(provider)
    return overridden_by


def classify_packages(
    selections: dict[str, PackageSelection],
    soong_modules: dict[str, SoongModule],
    make_paths: dict[str, list[str]],
    definitions: dict[str, list[str]],
    overridden_by: dict[str, list[SoongModule]],
    device: str,
) -> list[PackageResult]:
    results: list[PackageResult] = []
    for name, selection in selections.items():
        module = soong_modules.get(name)
        override_providers = sorted(overridden_by.get(name, []), key=lambda provider: provider.name)
        declared_in = sorted(selection.declared_in)
        if override_providers:
            module_paths = sorted({path for provider in override_providers for path in provider.module_paths})
            module_types = sorted({module_type for provider in override_providers for module_type in provider.module_types})
            definition_locations = sorted({location for provider in override_providers for location in definitions.get(provider.name, [])})
        else:
            module_paths = sorted(module.module_paths) if module else make_paths.get(name, [])
            module_types = sorted(module.module_types) if module else []
            definition_locations = definitions.get(name, [])
        priority = priority_for(module_paths, declared_in, device)
        if override_providers:
            status = "ready_overridden"
            reason = "removed by overrides from a selected Soong package"
        elif module is None:
            if any("Android.bp:" in location for location in definition_locations):
                status = "ready_disabled"
                reason = "defined in Android.bp but intentionally disabled or absent from the current Soong graph"
            elif any("Android.mk:" in location for location in definition_locations):
                status = "make_only"
                reason = "defined in Android.mk and unavailable to a Soong-only build"
            else:
                status = "ignored_unresolved"
                reason = "no matching module exists; Soong-only filesystem generation ignores this stale package entry"
        elif "soong_replacement" in module.module_types:
            status = "ready_replacement"
            reason = "Soong provides the generated -soong replacement used by filesystem generation"
        elif "hidden_soong" in module.module_types:
            status = "ready_hidden"
            reason = "Soong provides a hidden module for this product package"
        elif module.has_install:
            status = "ready"
            reason = "the current Soong graph provides an installed output"
        elif module.has_required or "phony" in module.module_types or "FAKE" in module.module_classes:
            status = "ready_aggregate"
            reason = "the current Soong graph provides a dependency aggregator"
        else:
            status = "ready_no_install"
            reason = "the Soong module exists and intentionally has no installed output"
        results.append(PackageResult(
            name=name,
            status=status,
            priority=priority,
            active=selection.active,
            selected_by=sorted(selection.selected_by),
            declared_in=declared_in,
            module_paths=module_paths,
            module_types=module_types,
            definition_locations=definition_locations,
            overridden_by=[provider.name for provider in override_providers],
            reason=reason,
        ))
    status_order = {
        "make_only": 0,
        "ignored_unresolved": 1,
        "ready_disabled": 2,
        "ready_no_install": 3,
        "ready_overridden": 4,
        "ready_replacement": 5,
        "ready_hidden": 6,
        "ready_aggregate": 7,
        "ready": 8,
    }
    priority_order = {"P0": 0, "P1": 1, "P2": 2}
    return sorted(results, key=lambda item: (status_order[item.status], priority_order[item.priority], item.name))


def resolve_product(product: str | None, device: str | None) -> str:
    if product:
        return product
    if device:
        return f"uwu_{device}"
    current = os.environ.get("TARGET_PRODUCT")
    if current:
        return current
    raise ValueError("specify --product or --device; TARGET_PRODUCT is not set")


def display_path(path: Path, source_root: Path) -> str:
    try:
        return str(path.relative_to(source_root))
    except ValueError:
        return str(path)


def analyze(
    source_root: Path,
    out_dir_arg: str,
    product_arg: str | None,
    device_arg: str | None,
    all_variants: bool,
) -> Analysis:
    product = resolve_product(product_arg, device_arg)
    out_dir = Path(out_dir_arg)
    if not out_dir.is_absolute():
        out_dir = source_root / out_dir
    soong_dir = out_dir / "soong"
    variables_path = soong_dir / f"soong.{product}.variables"
    extra_variables_path = soong_dir / f"soong.{product}.extra.variables"
    environment_path = soong_dir / f"soong.environment.used.{product}.build"
    android_mk_path = soong_dir / f"Android-{product}.mk"
    late_mk_path = soong_dir / f"late-{product}.mk"
    soong_module_info_path = soong_dir / f"module-info-{product}.json"
    kati_ninja_path = out_dir / f"build-{product}.ninja"
    package_ninja_path = out_dir / f"build-{product}-package.ninja"

    for path, description in (
        (variables_path, "product variables"),
        (android_mk_path, "Soong AndroidMk bridge"),
        (late_mk_path, "Soong late module bridge"),
        (kati_ninja_path, "Kati module graph"),
        (package_ninja_path, "Kati package graph"),
    ):
        if not path.is_file():
            raise ValueError(f"missing {description}: {path}; finish Ninja generation and Kati first")

    variables = load_json(variables_path)
    extra_variables = load_json(extra_variables_path) if extra_variables_path.is_file() else {}
    used_environment = parse_used_environment(environment_path)
    actual_product = str(variables.get("DeviceProduct", ""))
    if actual_product and actual_product != product:
        raise ValueError(f"{variables_path} describes {actual_product}, not {product}")
    device = str(variables.get("DeviceName", ""))
    if not device:
        raise ValueError(f"DeviceName is missing from {variables_path}")
    if device_arg and device != device_arg:
        raise ValueError(f"{product} targets device {device}, not {device_arg}")
    make_module_info_path = out_dir / "target" / "product" / device / "module-info.json"

    selections, shipping_api = collect_package_selections(
        variables, extra_variables, used_environment, all_variants
    )
    candidates = set(selections)
    soong_modules = parse_module_info(soong_module_info_path, candidates)
    merge_soong_modules(soong_modules, parse_soong_android_mk(android_mk_path, candidates))
    unresolved = candidates.difference(soong_modules)
    merge_soong_modules(soong_modules, parse_late_soong_modules(late_mk_path, unresolved))
    make_paths = parse_make_module_paths(make_module_info_path, candidates)
    unresolved = candidates.difference(soong_modules)
    override_providers = collect_override_providers(selections, soong_modules)
    definition_names: set[str] = set()
    for name in unresolved:
        providers = override_providers.get(name, [])
        if providers:
            definition_names.update(provider.name for provider in providers)
        else:
            definition_names.add(name)
    path_hints = {
        name: soong_modules[name].module_paths
        for name in definition_names
        if name in soong_modules
    }
    definitions = find_source_definitions(source_root, out_dir, definition_names, path_hints)
    warnings: list[str] = []
    if shipping_api is None and any(
        variables.get("PartitionVarsForSoongMigrationOnlyDoNotUse", {})
        .get("ProductPackagesSet", {})
        .get("all", {})
        .get(f"ProductPackagesShippingApiLevel{level}")
        for level in (29, 33, 34)
    ):
        warnings.append("shipping API level is unavailable; shipping-specific package lists were not selected")
    if android_mk_path.stat().st_mtime < variables_path.stat().st_mtime:
        warnings.append("Soong AndroidMk bridge is older than the product variables; regenerate the build graph")
    variant = "eng" if variables.get("Eng") else "userdebug" if variables.get("Debuggable") else "user"
    return Analysis(
        product=product,
        device=device,
        variant=variant,
        architecture=str(variables.get("DeviceArch", "")),
        shipping_api_level=shipping_api,
        inputs={
            "product_variables": display_path(variables_path, source_root),
            "soong_modules": display_path(android_mk_path, source_root),
            "soong_module_info": display_path(soong_module_info_path, source_root) if soong_module_info_path.is_file() else "",
            "soong_hidden_modules": display_path(late_mk_path, source_root),
            "kati_modules": display_path(kati_ninja_path, source_root),
            "kati_packaging": display_path(package_ninja_path, source_root),
        },
        packages=classify_packages(
            selections,
            soong_modules,
            make_paths,
            definitions,
            override_providers,
            device,
        ),
        warnings=warnings,
        all_variants=all_variants,
    )


def print_text(analysis: Analysis, show_ready: bool, language: str) -> None:
    summary = analysis.to_json()["summary"]
    zh = language == "zh"
    color = (
        sys.stdout.isatty()
        and not os.environ.get("NO_COLOR")
        and os.environ.get("UWU_NO_COLOR", "false") != "true"
        and os.environ.get("TERM", "dumb") != "dumb"
    )
    reset = "\033[0m" if color else ""
    bold = "\033[1m" if color else ""
    dim = "\033[2m" if color else ""
    marker_colors = {
        "PASS": "\033[32m" if color else "",
        "FAIL": "\033[31m" if color else "",
        "WARN": "\033[33m" if color else "",
        "INFO": "\033[36m" if color else "",
    }
    status_labels = {
        "make_only": "仅 Make" if zh else "Make only",
        "ignored_unresolved": "已忽略" if zh else "Ignored",
        "ready_disabled": "配置禁用" if zh else "Disabled by config",
        "ready_no_install": "无需安装" if zh else "No install output",
        "ready_overridden": "已被覆盖" if zh else "Overridden",
        "ready_replacement": "Soong 替代" if zh else "Soong replacement",
        "ready_hidden": "Soong 隐藏模块" if zh else "Hidden Soong module",
        "ready_aggregate": "依赖聚合" if zh else "Dependency aggregate",
        "ready": "已就绪" if zh else "Ready",
    }
    status_reasons = {
        "make_only": "仍由 Android.mk 定义，切换后会丢失该模块" if zh else "Still defined by Android.mk and would be lost after the switch",
        "ignored_unresolved": "未找到匹配模块；fsgen 会安全忽略该陈旧条目" if zh else "No matching module; fsgen safely ignores this stale entry",
        "ready_disabled": "Android.bp 模块在当前配置中禁用，fsgen 会忽略" if zh else "The Android.bp module is disabled for this configuration and fsgen ignores it",
        "ready_no_install": "模块存在，但不产生需要安装的输出" if zh else "The module exists but intentionally has no installed output",
        "ready_overridden": "由已选中的 Soong package 覆盖" if zh else "Overridden by another selected Soong package",
        "ready_replacement": "由 fsgen 使用的 Soong 替代模块提供" if zh else "Provided by the Soong replacement used by fsgen",
        "ready_hidden": "由 Soong 隐藏模块提供" if zh else "Provided by a hidden Soong module",
        "ready_aggregate": "由 Soong 依赖聚合模块提供" if zh else "Provided by a Soong dependency aggregate",
        "ready": "当前 Soong 图提供安装输出" if zh else "The current Soong graph provides an installed output",
    }

    section_started = False

    def section(title: str) -> None:
        nonlocal section_started
        if section_started:
            print()
        section_started = True
        print(f"{bold}{title}{reset}")
        print(f"{dim}{'-' * 20}{reset}")

    def metric(marker: str, label: str, value: str | int) -> None:
        print(f"  {marker_colors[marker]}[{marker}]{reset} {label}: {value}")

    def package_marker(marker: str) -> str:
        return f"{marker_colors[marker]}[{marker}]{reset}"

    section("目标" if zh else "Target")
    print(f"  {'产品' if zh else 'Product'}: {analysis.product}")
    print(f"  {'设备' if zh else 'Device'}: {analysis.device}")
    print(f"  {'构建' if zh else 'Build'}: {analysis.variant} / {analysis.architecture}")
    if analysis.shipping_api_level is not None:
        print(f"  {'首发 API' if zh else 'Shipping API'}: {analysis.shipping_api_level}")

    section("状态" if zh else "Status")
    metric("PASS", "已选 package" if zh else "Selected packages", summary["selected"])
    metric("PASS", "Soong 已提供" if zh else "Soong-backed", f"{summary['soong_backed']} / {summary['selected']}")
    metric("FAIL" if summary["blockers"] else "PASS", "Make 阻塞项" if zh else "Make blockers", summary["blockers"])
    metric("INFO", "已安全忽略" if zh else "Safely ignored", summary["ignored"])
    metric("WARN" if summary["warnings"] else "PASS", "输入警告" if zh else "Input warnings", summary["warnings"])

    visible = analysis.packages if show_ready else [
        package for package in analysis.packages if package.status == "make_only" or package.status.startswith("ignored")
    ]
    if visible:
        section("全部 package" if zh and show_ready else
                "需关注的 package" if zh else
                "All packages" if show_ready else "Packages to review")
        for package in visible:
            marker = "FAIL" if package.status == "make_only" else "INFO" if package.status.startswith("ignored") else "PASS"
            print(f"  {package_marker(marker)} {package.name}  {status_labels[package.status]} / {package.priority}")
            print(f"         {'选择来源' if zh else 'Selected by'}: {', '.join(package.selected_by)}")
            if package.declared_in:
                print(f"         {'声明位置' if zh else 'Declared in'}: {', '.join(package.declared_in)}")
            if package.module_paths:
                print(f"         {'模块路径' if zh else 'Module path'}: {', '.join(package.module_paths)}")
            if package.definition_locations:
                print(f"         {'定义位置' if zh else 'Defined at'}: {', '.join(package.definition_locations)}")
            if package.overridden_by:
                print(f"         {'覆盖模块' if zh else 'Overridden by'}: {', '.join(package.overridden_by)}")
            print(f"         {'说明' if zh else 'Reason'}: {status_reasons[package.status]}")
    if analysis.warnings:
        section("输入警告" if zh else "Input warnings")
        for warning in analysis.warnings:
            print(f"  {package_marker('WARN')} {warning}")

    section("结论" if zh else "Conclusion")
    if analysis.blockers:
        count = len(analysis.blockers)
        print(f"  {marker_colors['FAIL']}{'NOT READY / 暂不可切换' if zh else 'NOT READY'}{reset}")
        if zh:
            print(f"  启用 Soong-only 前需要迁移 {count} 个仍由 Android.mk 提供的 package。")
        else:
            print(f"  Migrate {count} package(s) still provided by Android.mk before enabling Soong-only.")
    elif analysis.warnings:
        print(f"  {marker_colors['WARN']}{'READY WITH WARNINGS / 可切换（有警告）' if zh else 'READY WITH WARNINGS'}{reset}")
        print("  package 已满足 Soong-only；请先检查上面的输入警告。" if zh else
              "  Package installation is Soong-only ready; review the input warnings above.")
    else:
        print(f"  {marker_colors['PASS']}{'READY / 可切换' if zh else 'READY'}{reset}")
        print("  没有选中的 package 仍依赖 Android.mk。" if zh else
              "  No selected package still depends on Android.mk.")
    if analysis.ignored_packages:
        count = len(analysis.ignored_packages)
        if zh:
            print(f"  fsgen 会安全忽略 {count} 个无匹配模块的陈旧条目。")
        else:
            print(f"  fsgen safely ignores {count} stale entr{'y' if count == 1 else 'ies'} with no matching module.")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-root", default=str(Path(__file__).resolve().parent.parent))
    parser.add_argument("--out-dir", default="out")
    target = parser.add_mutually_exclusive_group()
    target.add_argument("--product")
    target.add_argument("--device")
    parser.add_argument("--format", choices=("text", "json"), default="text")
    parser.add_argument("--all-variants", action="store_true")
    parser.add_argument("--show-ready", action="store_true")
    parser.add_argument("--language", choices=("en", "zh"), default="en")
    args = parser.parse_args()
    try:
        analysis = analyze(
            Path(args.source_root).resolve(),
            args.out_dir,
            args.product,
            args.device,
            args.all_variants,
        )
    except (OSError, ValueError) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 1
    if args.format == "json":
        print(json.dumps(analysis.to_json(), indent=2))
    else:
        print_text(analysis, args.show_ready, args.language)
    return 2 if analysis.blockers else 0


if __name__ == "__main__":
    raise SystemExit(main())
