#!/usr/bin/env python3

# Copyright (C) 2026 The uwuAOSP Project
# SPDX-License-Identifier: Apache-2.0

import json
import io
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path

import soong_only_packages


class SoongOnlyPackagesTest(unittest.TestCase):
    def make_tree(self, *, debuggable: bool = False, eng: bool = False) -> tuple[Path, Path]:
        root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        out = root / "out"
        soong = out / "soong"
        product_out = out / "target/product/foo"
        soong.mkdir(parents=True)
        product_out.mkdir(parents=True)
        variables = {
            "DeviceName": "foo",
            "DeviceProduct": "uwu_foo",
            "DeviceArch": "arm64",
            "DeviceSecondaryArch": "",
            "Debuggable": debuggable,
            "Eng": eng,
            "SanitizeDevice": [],
            "PartitionVarsForSoongMigrationOnlyDoNotUse": {
                "ProductPackagesSet": {
                    "all": {
                        "ProductPackages": [
                            "ready",
                            "aggregate",
                            "warning",
                            "hidden",
                            "replaced",
                            "override_provider",
                            "overridden",
                            "disabled_soong",
                            "stale_unresolved",
                            "make_only",
                        ],
                        "ProductPackagesDebug": ["debug_only"],
                        "ProductPackagesEng": ["eng_only"],
                        "ProductPackagesArm64": ["arm64_only"],
                        "ProductPackagesShippingApiLevel29": ["shipping_29"],
                        "ProductPackagesShippingApiLevel33": ["shipping_33"],
                        "ProductPackagesShippingApiLevel34": ["shipping_34"],
                    },
                    "device/vendor/foo/device.mk": {
                        "ProductPackages": ["make_only"],
                    },
                }
            },
        }
        (soong / "soong.uwu_foo.variables").write_text(json.dumps(variables))
        (soong / "soong.uwu_foo.extra.variables").write_text(json.dumps({"ShippingApiLevel": "33"}))
        (soong / "soong.environment.used.uwu_foo.build").write_text("EMMA_INSTRUMENT='false'\n")
        (soong / "Android-uwu_foo.mk").write_text("""\
include $(CLEAR_VARS)  # phony.phony
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := aggregate
include $(BUILD_PHONY_PACKAGE)
include $(CLEAR_VARS)
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := arm64_only
LOCAL_SOONG_MODULE_TYPE := cc_binary
LOCAL_SOONG_INSTALLED_MODULE := out/target/product/foo/system/bin/arm64_only
include $(BUILD_PREBUILT)
include $(CLEAR_VARS)  # type: android_app, name: override_provider, variant: android_common
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := override_provider
LOCAL_SOONG_MODULE_TYPE := android_app
LOCAL_SOONG_INSTALLED_MODULE := out/target/product/foo/system/app/override_provider/override_provider.apk
LOCAL_OVERRIDES_PACKAGES := overridden
include $(BUILD_PREBUILT)
include $(CLEAR_VARS)
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := ready
LOCAL_SOONG_MODULE_TYPE := android_app
LOCAL_SOONG_INSTALLED_MODULE := out/target/product/foo/system/app/ready/ready.apk
include $(BUILD_PREBUILT)
include $(CLEAR_VARS)
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := shipping_33
LOCAL_SOONG_MODULE_TYPE := prebuilt_etc
LOCAL_SOONG_INSTALLED_MODULE := out/target/product/foo/system/etc/shipping_33
include $(BUILD_PREBUILT)
include $(CLEAR_VARS)
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := shipping_34
LOCAL_SOONG_MODULE_TYPE := prebuilt_etc
LOCAL_SOONG_INSTALLED_MODULE := out/target/product/foo/system/etc/shipping_34
include $(BUILD_PREBUILT)
include $(CLEAR_VARS)  # type: install_symlink, name: warning, variant: android_common
LOCAL_PATH := device/vendor/foo
LOCAL_MODULE := warning
LOCAL_SOONG_INSTALL_SYMLINKS := out/target/product/foo/system/warning
include $(BUILD_PREBUILT)
""")
        (soong / "late-uwu_foo.mk").write_text("""\
.PHONY: hidden-soong
hidden-soong:
.PHONY: replaced-soong-soong
replaced-soong-soong:
""")
        module_paths = out / ".module_paths"
        module_paths.mkdir()
        source_device = root / "device/vendor/foo"
        source_device.mkdir(parents=True)
        (source_device / "Android.bp").write_text("""\
android_app {
    name: "override_provider",
}
cc_binary {
    name: "disabled_soong",
    enabled: false,
}
""")
        (source_device / "Android.mk").write_text("""\
include $(CLEAR_VARS)
LOCAL_MODULE := make_only
include $(BUILD_PREBUILT)
""")
        (module_paths / "Android.bp.list").write_text("device/vendor/foo/Android.bp\n")
        (module_paths / "Android.mk.list").write_text("device/vendor/foo/Android.mk\n")
        (out / "build-uwu_foo.ninja").write_text("# kati\n")
        (out / "build-uwu_foo-package.ninja").write_text("# packaging\n")
        return root, out

    def analyze(self, root: Path, all_variants: bool = False) -> soong_only_packages.Analysis:
        return soong_only_packages.analyze(root, "out", "uwu_foo", None, all_variants)

    def test_classifies_current_variant_packages(self):
        root, _ = self.make_tree()
        analysis = self.analyze(root)
        packages = {package.name: package for package in analysis.packages}

        self.assertEqual("ready", packages["ready"].status)
        self.assertEqual("ready_aggregate", packages["aggregate"].status)
        self.assertEqual("ready", packages["warning"].status)
        self.assertEqual("ready_hidden", packages["hidden"].status)
        self.assertEqual("ready_replacement", packages["replaced"].status)
        self.assertEqual("ready_overridden", packages["overridden"].status)
        self.assertEqual(["override_provider"], packages["overridden"].overridden_by)
        self.assertEqual(["device/vendor/foo/Android.bp:2"], packages["overridden"].definition_locations)
        self.assertEqual("make_only", packages["make_only"].status)
        self.assertEqual(["device/vendor/foo/Android.mk:2"], packages["make_only"].definition_locations)
        self.assertEqual("ready_disabled", packages["disabled_soong"].status)
        self.assertEqual(["device/vendor/foo/Android.bp:5"], packages["disabled_soong"].definition_locations)
        self.assertEqual("ignored_unresolved", packages["stale_unresolved"].status)
        self.assertEqual("P0", packages["make_only"].priority)
        self.assertIn("arm64_only", packages)
        self.assertIn("shipping_33", packages)
        self.assertIn("shipping_34", packages)
        self.assertNotIn("shipping_29", packages)
        self.assertNotIn("debug_only", packages)

    def test_debug_and_eng_packages_follow_current_variant(self):
        root, _ = self.make_tree(debuggable=True, eng=True)
        analysis = self.analyze(root)
        names = {package.name for package in analysis.packages}

        self.assertIn("debug_only", names)
        self.assertIn("eng_only", names)

    def test_all_variants_includes_inactive_packages(self):
        root, _ = self.make_tree()
        analysis = self.analyze(root, all_variants=True)
        packages = {package.name: package for package in analysis.packages}

        self.assertIn("debug_only", packages)
        self.assertFalse(packages["debug_only"].active)

    def test_device_resolves_uwu_product(self):
        root, _ = self.make_tree()
        analysis = soong_only_packages.analyze(root, "out", None, "foo", False)

        self.assertEqual("uwu_foo", analysis.product)
        self.assertEqual("foo", analysis.device)

    def test_missing_kati_output_is_rejected(self):
        root, out = self.make_tree()
        (out / "build-uwu_foo-package.ninja").unlink()

        with self.assertRaisesRegex(ValueError, "finish Ninja generation and Kati first"):
            self.analyze(root)

    def test_json_package_order_is_stable(self):
        root, _ = self.make_tree()
        analysis = self.analyze(root)
        first = json.dumps(analysis.to_json(), sort_keys=True)
        second = json.dumps(self.analyze(root).to_json(), sort_keys=True)

        self.assertEqual(first, second)

    def test_array_module_info_merges_variants(self):
        root, out = self.make_tree()
        module_info = out / "soong/module-info-uwu_foo.json"
        module_info.write_text(json.dumps([
            {"ready": {"path": ["device/vendor/foo"], "uninstallable": True}},
            {"ready": {"installed": ["out/target/product/foo/system/app/ready.apk"]}},
        ]))

        modules = soong_only_packages.parse_module_info(module_info, {"ready"})

        self.assertEqual(2, modules["ready"].variants)
        self.assertTrue(modules["ready"].has_install)
        self.assertEqual({"device/vendor/foo"}, modules["ready"].module_paths)

    def test_analysis_merges_module_info_with_android_mk_metadata(self):
        root, out = self.make_tree()
        (out / "soong/module-info-uwu_foo.json").write_text(json.dumps([
            {"override_provider": {"path": ["device/vendor/foo"]}},
        ]))

        packages = {package.name: package for package in self.analyze(root).packages}

        self.assertEqual("ready_overridden", packages["overridden"].status)
        self.assertEqual(["override_provider"], packages["overridden"].overridden_by)

    def test_text_report_highlights_findings_and_conclusion(self):
        root, _ = self.make_tree()
        output = io.StringIO()

        with redirect_stdout(output):
            soong_only_packages.print_text(self.analyze(root), False, "en")

        report = output.getvalue()
        self.assertIn("Packages to review", report)
        self.assertIn("[FAIL] make_only", report)
        self.assertIn("[INFO] stale_unresolved", report)
        self.assertIn("NOT READY", report)
        self.assertNotIn("[PASS] ready  Ready", report)


if __name__ == "__main__":
    unittest.main()
