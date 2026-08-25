#!/usr/bin/env python3

# Copyright (C) 2026 The uwuAOSP Project
# SPDX-License-Identifier: Apache-2.0

import tempfile
import unittest
from pathlib import Path

import kernel_migrate


class KernelMigrateTest(unittest.TestCase):
    def make_tree(self, board: str) -> tuple[Path, Path]:
        root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        device = root / "device/vendor/foo"
        device.mkdir(parents=True)
        (device / "BoardConfigCommon.mk").write_text(board)
        (device / "Android.bp").write_text("soong_namespace {}\n")
        (device / "common.mk").write_text("PRODUCT_PACKAGES += init.foo\n")
        return root, device

    def test_simple_source_kernel_is_convertible(self):
        root, device = self.make_tree("""\
COMMON_PATH := device/vendor/foo
TARGET_ARCH := arm64
TARGET_KERNEL_VERSION := 5.10
BOARD_KERNEL_IMAGE_NAME := Image.gz
BOARD_KERNEL_PAGESIZE := 4096
TARGET_KERNEL_SOURCE := kernel/vendor/foo
TARGET_KERNEL_CONFIG := foo_defconfig vendor/foo.config
TARGET_NEEDS_DTBOIMAGE := true
""")
        migration = kernel_migrate.analyze(root, str(device), None)
        self.assertEqual([], migration.blockers)
        self.assertIn('kernel_dir: "kernel/vendor/foo"', migration.block)
        self.assertIn('clang_version: "clang-r563880c"', migration.block)
        self.assertIn('defconfig: "foo_defconfig"', migration.block)
        self.assertIn('fragments:', migration.block)
        self.assertIn('dtbo:', migration.block)

    def test_gki_merge_dt_and_blocklists_are_converted(self):
        root, device = self.make_tree("""\
COMMON_PATH := device/vendor/foo
TARGET_ARCH := arm64
TARGET_KERNEL_VERSION := 5.15
BOARD_KERNEL_IMAGE_NAME := Image
TARGET_KERNEL_SOURCE := kernel/vendor/foo
TARGET_KERNEL_CONFIG := gki_defconfig vendor/foo.config
BOARD_USES_QCOM_MERGE_DTBS_SCRIPT := true
BOARD_INCLUDE_DTB_IN_BOOTIMG := true
TARGET_NEEDS_DTBOIMAGE := true
BOARD_VENDOR_KERNEL_MODULES_BLOCKLIST_FILE := $(TARGET_KERNEL_SOURCE)/modules.blocklist
BOARD_VENDOR_RAMDISK_KERNEL_MODULES_BLOCKLIST_FILE := $(BOARD_VENDOR_KERNEL_MODULES_BLOCKLIST_FILE)
BOARD_VENDOR_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load))
BOOT_KERNEL_MODULES := $(strip $(shell cat $(COMMON_PATH)/modules.include.vendor_ramdisk))
BOARD_VENDOR_RAMDISK_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.first_stage))
BOARD_VENDOR_RAMDISK_RECOVERY_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.first_stage $(COMMON_PATH)/modules.load.recovery))
""")
        (device / "modules.load").write_text("vendor.ko\n")
        (device / "modules.include.vendor_ramdisk").write_text("early.ko\nrecovery.ko\n")
        (device / "modules.load.first_stage").write_text("early.ko\n")
        (device / "modules.load.recovery").write_text("recovery.ko\n")
        migration = kernel_migrate.analyze(root, str(device), None)
        self.assertEqual([], migration.blockers)
        self.assertIn("qcom_merge: true", migration.block)
        self.assertIn('vendor_dlkm_module_blocklist: "kernel/vendor/foo/modules.blocklist"', migration.block)
        self.assertIn('vendor_ramdisk_module_blocklist: "kernel/vendor/foo/modules.blocklist"', migration.block)
        self.assertIn('recovery_module_load_list:', migration.block)

    def test_nested_strip_shell_cat_module_list(self):
        root, device = self.make_tree("""\
COMMON_PATH := device/vendor/foo
TARGET_ARCH := arm64
TARGET_KERNEL_VERSION := 5.15
BOARD_KERNEL_IMAGE_NAME := Image
TARGET_KERNEL_SOURCE := kernel/vendor/foo
TARGET_KERNEL_CONFIG := gki_defconfig
SYSTEM_KERNEL_MODULES := $(strip $(shell cat $(COMMON_PATH)/modules.include.system_dlkm))
BOARD_SYSTEM_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.system_dlkm))
""")
        (device / "modules.include.system_dlkm").write_text("zram.ko\nzsmalloc.ko\n")
        (device / "modules.load.system_dlkm").write_text("zram.ko\n")
        migration = kernel_migrate.analyze(root, str(device), None)
        self.assertEqual([], migration.blockers)
        self.assertIn('system_dlkm_module_install_list:', migration.block)
        self.assertIn('"device/vendor/foo/modules.include.system_dlkm"', migration.block)

    def test_device_inherits_common_kernel_and_keeps_device_flags(self):
        root = Path(self.enterContext(tempfile.TemporaryDirectory()))
        common = root / "device/vendor/sm8550-common"
        device = root / "device/vendor/astonc"
        common.mkdir(parents=True)
        device.mkdir(parents=True)
        (common / "BoardConfigCommon.mk").write_text("""\
COMMON_PATH := device/vendor/sm8550-common
TARGET_ARCH := arm64
TARGET_KERNEL_VERSION := 5.15
BOARD_KERNEL_IMAGE_NAME := Image
TARGET_KERNEL_SOURCE := kernel/vendor/sm8550
TARGET_KERNEL_CONFIG := gki_defconfig vendor/common.config
TARGET_KERNEL_ADDITIONAL_FLAGS := CONFIG_COMMON_DTBS=y
SYSTEM_KERNEL_MODULES := $(strip $(shell cat $(COMMON_PATH)/modules.include.system_dlkm))
BOARD_SYSTEM_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.system_dlkm))
""")
        (common / "modules.include.system_dlkm").write_text("zram.ko\n")
        (common / "modules.load.system_dlkm").write_text("zram.ko\n")
        (device / "BoardConfig.mk").write_text("""\
include device/vendor/sm8550-common/BoardConfigCommon.mk
TARGET_KERNEL_ADDITIONAL_FLAGS += CONFIG_ASTON_DTB=y
""")
        (device / "Android.bp").write_text("soong_namespace {}\n")
        (device / "device.mk").write_text("PRODUCT_PACKAGES += init.astonc\n")

        migration = kernel_migrate.analyze(root, "astonc", None)

        self.assertEqual(Path("device/vendor/astonc"), migration.device_dir)
        self.assertEqual([], migration.blockers)
        self.assertIn('"CONFIG_COMMON_DTBS=y"', migration.block)
        self.assertIn('"CONFIG_ASTON_DTB=y"', migration.block)
        self.assertIn('"device/vendor/sm8550-common/modules.include.system_dlkm"', migration.block)
        kernel_migrate.apply(migration)
        self.assertIn("uwu_kernel {", (device / "Android.bp").read_text())
        self.assertIn("//device/vendor/astonc:kernel", (device / "BoardConfig.mk").read_text())
        self.assertIn("TARGET_KERNEL_SOURCE", (common / "BoardConfigCommon.mk").read_text())
        self.assertIn("kernel", (device / "device.mk").read_text())

    def test_apply_updates_device_files(self):
        root, device = self.make_tree("""\
TARGET_ARCH := arm64
TARGET_KERNEL_VERSION := 5.10
BOARD_KERNEL_IMAGE_NAME := Image.gz
TARGET_KERNEL_SOURCE := kernel/vendor/foo
TARGET_KERNEL_CONFIG := foo_defconfig
""")
        migration = kernel_migrate.analyze(root, str(device), None)
        kernel_migrate.apply(migration)
        self.assertIn("uwu_kernel {", (device / "Android.bp").read_text())
        board = (device / "BoardConfigCommon.mk").read_text()
        self.assertIn("BOARD_USES_SOONG_KERNEL := true", board)
        self.assertNotIn("TARGET_KERNEL_SOURCE", board)
        self.assertIn("kernel", (device / "common.mk").read_text())

    def test_infers_kernel_version_and_dlkm_partitions(self):
        root, device = self.make_tree("""\
TARGET_ARCH := arm64
BOARD_KERNEL_IMAGE_NAME := Image
TARGET_KERNEL_SOURCE := kernel/vendor/foo
TARGET_KERNEL_CONFIG := gki_defconfig
BOARD_SYSTEM_DLKMIMAGE_FILE_SYSTEM_TYPE := erofs
BOARD_VENDOR_DLKMIMAGE_FILE_SYSTEM_TYPE := erofs
COMMON_PATH := device/vendor/foo
BOARD_VENDOR_RAMDISK_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.first_stage))
BOOT_KERNEL_MODULES := $(strip $(shell cat $(COMMON_PATH)/modules.include.vendor_ramdisk))
SYSTEM_KERNEL_MODULES := $(strip $(shell cat $(COMMON_PATH)/modules.include.system_dlkm))
BOARD_SYSTEM_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.system_dlkm))
""")
        (device / "modules.load.first_stage").write_text("early.ko\n")
        (device / "modules.include.vendor_ramdisk").write_text("early.ko\n")
        (device / "modules.include.system_dlkm").write_text("zram.ko\n")
        (device / "modules.load.system_dlkm").write_text("zram.ko\n")
        kernel = root / "kernel/vendor/foo"
        kernel.mkdir(parents=True)
        (kernel / "Makefile").write_text("VERSION = 5\nPATCHLEVEL = 15\n")
        migration = kernel_migrate.analyze(root, str(device), None)
        self.assertEqual("5.15", migration.inferred_kernel_version)
        self.assertIn("system_dlkm_module_install_list", migration.block)
        self.assertIn("vendor_ramdisk_module_install_list", migration.block)
        kernel_migrate.apply(migration)
        self.assertIn("TARGET_KERNEL_VERSION := 5.15", (device / "BoardConfigCommon.mk").read_text())

    def test_apply_removes_migrated_module_variables(self):
        root, device = self.make_tree("""\
COMMON_PATH := device/vendor/foo
TARGET_ARCH := arm64
TARGET_KERNEL_VERSION := 5.15
BOARD_KERNEL_IMAGE_NAME := Image
TARGET_KERNEL_SOURCE := kernel/vendor/foo
TARGET_KERNEL_CONFIG := gki_defconfig
SYSTEM_KERNEL_MODULES := $(strip $(shell cat $(COMMON_PATH)/modules.include.system_dlkm))
BOARD_SYSTEM_KERNEL_MODULES_LOAD := $(strip $(shell cat $(COMMON_PATH)/modules.load.system_dlkm))
""")
        (device / "modules.include.system_dlkm").write_text("zram.ko\n")
        (device / "modules.load.system_dlkm").write_text("zram.ko\n")
        migration = kernel_migrate.analyze(root, str(device), None)
        kernel_migrate.apply(migration)
        board = (device / "BoardConfigCommon.mk").read_text()
        self.assertNotIn("SYSTEM_KERNEL_MODULES", board)
        self.assertNotIn("BOARD_SYSTEM_KERNEL_MODULES_LOAD", board)


if __name__ == "__main__":
    unittest.main()
