#!/usr/bin/env python3
"""Regression coverage for the Debian/vendor media page-layout boundary."""

import hashlib
import importlib.util
import pathlib
import shutil
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/debian-system/prepare-media-compat.py"
HELPER = SCRIPT.with_name("media-page-compat.c")
SPEC = importlib.util.spec_from_file_location("prepare_media_compat", SCRIPT)
COMPAT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(COMPAT)


class MediaPageCompatTest(unittest.TestCase):
    def test_helpers_use_configured_page_size_and_scatterlist_offset(self):
        """The production helper must work for vendor and Debian page layouts."""
        with tempfile.TemporaryDirectory() as temporary:
            work = pathlib.Path(temporary)
            headers = work / "linux"
            headers.mkdir()
            (headers / "mm.h").write_text("""
struct page { unsigned char storage[TEST_PAGE_SIZE]; };
static struct page test_pages[32];
#define TEST_PFN_BASE 0xaaf0UL
#define pfn_to_page(pfn) (test_pages + ((pfn) - TEST_PFN_BASE))
""")
            (headers / "scatterlist.h").write_text("""
struct scatterlist { struct page *page; unsigned int offset; };
#define sg_phys(sg) ((((sg)->page - test_pages + TEST_PFN_BASE) << 12) + (sg)->offset)
""")
            (headers / "module.h").write_text("""
#define module_param(name, type, mode)
#define MODULE_PARM_DESC(name, text)
#define MODULE_INFO(name, text)
""")
            shutil.copyfile(str(HELPER), str(work / "helper.c"))
            (work / "harness.c").write_text("""
#include <assert.h>
#include "helper.c"
int main(void) {
    unsigned long pfn;
    assert(aiden_page_size == TEST_PAGE_SIZE);
    for (pfn = TEST_PFN_BASE; pfn < TEST_PFN_BASE + 32; pfn++) {
        struct scatterlist sg = { pfn_to_page(pfn), 123 };
        assert(aiden_media_pfn_to_page(pfn) == &test_pages[pfn - TEST_PFN_BASE]);
        assert(aiden_media_sg_phys(&sg) == (pfn << 12) + 123);
    }
    return 0;
}
""")
            for page_size in (32, 36, 40, 64):
                executable = work / ("page-" + str(page_size))
                subprocess.run(["gcc", "-std=c11", "-I", str(work),
                                "-DTEST_PAGE_SIZE=" + str(page_size),
                                str(work / "harness.c"), "-o", str(executable)], check=True)
                subprocess.run([str(executable)], check=True)

    def test_staging_is_repeatable_and_preserves_vendor_sources(self):
        sdk = ROOT / "pico-sdk"
        releases = [sdk / COMPAT.KMPP_RELEASE, sdk / COMPAT.ROCKIT_RELEASE]
        original = {path: hashlib.sha256(path.read_bytes()).digest()
                    for release in releases for path in release.rglob("*.S")}
        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary) / "media"
            COMPAT.stage(sdk, output)
            first = {path.relative_to(output): path.read_bytes()
                     for path in output.rglob("*") if path.is_file()}
            COMPAT.stage(sdk, output)
            second = {path.relative_to(output): path.read_bytes()
                      for path in output.rglob("*") if path.is_file()}
            self.assertEqual(first, second)
            calls = sum(data.count(b"\tbl\taiden_media_pfn_to_page\n")
                        for path, data in first.items() if path.suffix == ".S")
            self.assertEqual(calls, 3)
            valloc = (output / "rockit/valloc/valloc.S").read_text()
            self.assertIn("\tbl\taiden_media_sg_phys\n", valloc)
            self.assertNotIn("\tadd\tr4, r3, r4, asr #5\n", valloc)
            self.assertIn("mpp_vcodec-y += kmpp/mpp/mpp_service.o\n", first[pathlib.Path("Makefile")].decode())
            self.assertIn("rockit-y += rockit/fakecode.o\n", first[pathlib.Path("Makefile")].decode())
            for path in (output / "kmpp/vcodec/mpp_vcodec_driver.S", output / "rockit/rockit_init.S"):
                self.assertIn("\t.thumb_set\tinit_module,", path.read_text())
                self.assertIn('"license=GPL\\000"', path.read_text())
            self.assertIn("\t.word\t__this_module\n", (output / "kmpp/vcodec/mpp_vcodec_driver.S").read_text())
            self.assertIn("procfs_fops_u32:\n\t.space\t4\n\t.word\tfops_open_u32\n",
                          (output / "kmpp/mpp/mpp_common.S").read_text())
            for source, name in (("valloc/valloc.S", "valloc_driver_fops"),
                                 ("venc/venc.S", "venc_driver_fops")):
                self.assertIn(name + ":\n\t.word\t__this_module\n\t.space\t4\n",
                              (output / "rockit" / source).read_text())
            self.assertNotIn("__modver_version_show", (output / "rockit/mpi/mpi_dev.S").read_text())
            self.assertIn('"version=0.1\\000"', (output / "rockit/mpi/mpi_dev.S").read_text())
        self.assertEqual(original, {path: hashlib.sha256(path.read_bytes()).digest() for path in original})

    def test_module_owner_preserves_proc_ops_flags(self):
        # proc_ops names can resemble file_operations names. Their first field
        # is a flags word, so size44 objects must remain unchanged in all cases.
        for name in ("procfs_fops_u32", "procfs_fops"):
            proc_ops = ("\t.type\t{name}, %object\n\t.size\t{name}, 44\n"
                        "{name}:\n\t.space\t4\n\t.word\tfops_open_u32\n").format(name=name)
            self.assertEqual(COMPAT.module_assembly(proc_ops), proc_ops)
        file_operations = ("\t.type\tvcodec_fops, %object\n"
                           "\t.size\tvcodec_fops, 128\nvcodec_fops:\n"
                           "\t.space\t36\n\t.word\tvcodec_poll\n")
        self.assertIn("vcodec_fops:\n\t.word\t__this_module\n\t.space\t32\n",
                      COMPAT.module_assembly(file_operations))
        explicit_word = file_operations.replace("\t.space\t36\n", "\t.word\t0\n\t.space\t32\n")
        self.assertEqual(COMPAT.module_assembly(explicit_word),
                         COMPAT.module_assembly(file_operations))
        proc_ops_word = explicit_word.replace(
            "\t.size\tvcodec_fops, 128\n", "\t.size\tvcodec_fops, 44\n")
        self.assertEqual(COMPAT.module_assembly(proc_ops_word), proc_ops_word)

    def test_upstream_changes_fail_closed(self):
        with self.assertRaisesRegex(ValueError, "Expected exactly one"):
            COMPAT.replace_page_conversion("upstream layout changed", ".L229", 16, "r9")

    def test_output_guards_preserve_existing_data(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary)
            important = output / "keep.txt"
            important.write_text("keep")
            with self.assertRaisesRegex(ValueError, "unrecognized output"):
                COMPAT.stage(ROOT / "pico-sdk", output)
            self.assertEqual(important.read_text(), "keep")
        with self.assertRaisesRegex(ValueError, "outside the SDK"):
            COMPAT.stage(ROOT / "pico-sdk", ROOT)


if __name__ == "__main__":
    unittest.main()
