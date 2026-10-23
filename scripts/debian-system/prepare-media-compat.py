#!/usr/bin/env python3
"""Stage RV1106 media modules with kernel-configured struct page arithmetic.

The SDK's assembly release is built-in code, while Debian loads these drivers
as modules. Besides replacing the four page conversions, staging restores the
module entry points, metadata and parameter ownership. The SDK is read-only.
"""

import argparse
import json
import pathlib
import re
import shutil
import subprocess
import sys


KMPP_RELEASE = "sysdrv/drv_ko/kmpp/release_kmpp_rv1106_arm_asm"
ROCKIT_RELEASE = "sysdrv/drv_ko/rockit/release_rockit-ko_rv1106_arm_asm"


def replace_once(source, old, new, description):
    if source.count(old) != 1:
        raise ValueError("Expected exactly one {}".format(description))
    return source.replace(old, new, 1)


def replace_page_conversion(source, pool, offset, register):
    old = (
        "\tldr\tr3, {pool}+{offset}\n"
        "\tldr\tr2, [r3]\n"
        "\tldr\tr3, {pool}+{next_offset}\n"
        "\trsb\tr2, r2, {register}, lsr #12\n"
        "\tadd\t{register}, {register}, #4096\n"
        "\tldr\tr3, [r3]\n"
        "\tadd\tr3, r3, r2, lsl #5\n"
    ).format(pool=pool, offset=offset, next_offset=offset + 4, register=register)
    new = (
        "\tlsr\tr0, {register}, #12\n"
        "\tbl\taico_media_pfn_to_page\n"
        "\tmov\tr3, r0\n"
        "\tadd\t{register}, {register}, #4096\n"
    ).format(register=register)
    return replace_once(source, old, new, "{} PFN conversion".format(register))


def patch_page_conversions(kmpp, rockit):
    path = kmpp / "vcodec/mpp/base/mpp_buffer.S"
    source = path.read_text()
    source = replace_page_conversion(source, ".L229", 16, "r9")
    source = replace_page_conversion(source, ".L229", 16, "r7")
    path.write_text(source)

    path = kmpp / "vcodec/mpp/base/mpp_packet.S"
    source = replace_page_conversion(path.read_text(), ".L68", 28, "r6")
    path.write_text(source)

    path = rockit / "valloc/valloc.S"
    old = (
        "\tldr\tr4, [r0]\n"
        "\tadds\tr7, r7, #1\n"
        "\tldr\tr3, [r9]\n"
        "\tbic\tr4, r4, #3\n"
        "\tldr\tr2, [r0, #4]\n"
        "\tsubs\tr4, r4, r3\n"
        "\tldr\tr3, [r10]\n"
        "\tadd\tr4, r3, r4, asr #5\n"
        "\tadd\tr4, r2, r4, lsl #12\n"
    )
    # r0 is also the next sg_next() argument; preserve it across the helper.
    # Four saved registers retain the ARM EABI's eight-byte stack alignment.
    new = (
        "\tpush\t{r0, r1, r2, r3}\n"
        "\tbl\taico_media_sg_phys\n"
        "\tmov\tr4, r0\n"
        "\tpop\t{r0, r1, r2, r3}\n"
        "\tadds\tr7, r7, #1\n"
    )
    path.write_text(replace_once(path.read_text(), old, new, "scatterlist conversion"))

    # MODULE_VERSION emits these extra sysfs objects only for built-in code.
    # Loadable modules use the retained version= entry in .modinfo instead.
    path = rockit / "mpi/mpi_dev.S"
    old = (
        "___modver_attr:\n\t.word\t.LC35\n\t.short\t292\n\t.space\t2\n"
        "\t.word\t__modver_version_show\n\t.space\t16\n"
        "\t.word\t.LC36\n\t.word\t.LC37\n"
    )
    source = replace_once(path.read_text(), old, "___modver_attr:\n\t.space\t36\n",
                          "built-in module version attribute")
    source = replace_once(source, "__modver_attr:\n\t.word\t___modver_attr\n",
                          "__modver_attr:\n\t.space\t4\n", "built-in module version pointer")
    path.write_text(source)


def module_assembly(source):
    # Preserve all object sizes/anchor offsets when removing built-in prefixes.
    # Padding belongs after the full string, which can span several .ascii lines.
    pattern = re.compile(
        r'(?P<head>\t\.ascii\t")(?P<prefix>mpp_vcodec\.|rk_vcodec\.|rockit\.)'
        r'(?P<body>[^\n]*\n(?:\t\.ascii\t[^\n]*\n)*)'
    )

    def unprefix(match):
        return (match.group("head") + match.group("body") +
                "\t.space\t{}\n".format(len(match.group("prefix"))))

    source = pattern.sub(unprefix, source)
    # Built-in parameters have no owner; loadable parameters must retain their
    # module while a sysfs operation is in progress.
    source = re.sub(
        r'(__param_[A-Za-z0-9_]+:\n\t\.word\t__param_str_[^\n]+\n)\t\.word\t0\n',
        r'\1\t.word\t__this_module\n', source,
    )
    # The RV1106 release's struct file_operations is 128 bytes and has owner
    # first. proc_ops can also have names ending in fops, but starts with flags;
    # require the known file_operations layout before restoring module ownership.
    def file_owner(match):
        if match.group("size") is None:
            return match.group("head") + "\t.word\t__this_module\n"
        size = int(match.group("size"))
        if size < 4:
            raise ValueError("Unexpected file_operations owner allocation")
        return (match.group("head") + "\t.word\t__this_module\n" +
                "\t.space\t{}\n".format(size - 4))

    source = re.sub(
        r'(?P<head>\t\.type\t(?P<name>[A-Za-z0-9_]*fops), %object\n'
        r'\t\.size\t(?P=name), 128\n(?P=name):\n)'
        r'(?:\t\.space\t(?P<size>[0-9]+)\n|\t\.word\t0\n)',
        file_owner, source,
    )
    return source


def object_files(makefile, variable):
    objects = re.findall(
        r"^" + re.escape(variable) + r"-objs\s*\+=\s*(\S+)\s*$",
        makefile.read_text(), re.MULTILINE,
    )
    # fakecode lives outside the vendor assembly directory and is added below.
    return [name[2:] if name.startswith("./") else name
            for name in objects if not name.startswith("../")]


def stage(sdk, output):
    sdk = sdk.resolve()
    output = output.resolve()
    if output == sdk or sdk in output.parents or output in sdk.parents:
        raise ValueError("Media compatibility output must be outside the SDK")
    releases = [("kmpp", sdk / KMPP_RELEASE), ("rockit", sdk / ROCKIT_RELEASE)]
    for _, release in releases:
        if not (release / "Makefile").is_file():
            raise ValueError("Missing vendor media assembly: {}".format(release))
    if output.exists() and not (output / ".aico-media-compat").is_file():
        raise ValueError("Refusing to replace an unrecognized output directory: {}".format(output))
    if output.exists():
        shutil.rmtree(str(output))
    output.mkdir(parents=True)
    (output / ".aico-media-compat").write_text("1\n")
    for name, release in releases:
        shutil.copytree(str(release), str(output / name))
    kmpp, rockit = output / "kmpp", output / "rockit"
    patch_page_conversions(kmpp, rockit)
    for directory in (kmpp, rockit):
        for path in directory.rglob("*.S"):
            path.write_text(module_assembly(path.read_text()))
        shutil.copyfile(str(pathlib.Path(__file__).with_name("media-page-compat.c")),
                        str(directory / "aico-media-pages.c"))

    for path, init, exit_ in (
        (kmpp / "vcodec/mpp_vcodec_driver.S", "vcodec_init", "vcodec_exit"),
        (rockit / "rockit_init.S", "rockit_load", "rockit_exit"),
    ):
        source = path.read_text()
        for function in (init, exit_):
            if not re.search(r"^" + function + r":$", source, re.MULTILINE):
                raise ValueError("Missing module entry point: {}".format(function))
        source += ("\n\t.global\tinit_module\n\t.thumb_set\tinit_module, {}\n"
                   "\t.global\tcleanup_module\n\t.thumb_set\tcleanup_module, {}\n").format(init, exit_)
        path.write_text(source)

    shutil.copyfile(str(sdk / "sysdrv/drv_ko/rockit/fakecode.c"),
                    str(rockit / "fakecode.c"))
    lines = ["# Generated by prepare-media-compat.py; SDK source is unchanged.",
             "obj-m += mpp_vcodec.o rockit.o"]
    for module, directory, variable in (("mpp_vcodec", kmpp, "kmpp"),
                                        ("rockit", rockit, "rockit-ko")):
        objects = object_files(directory / "Makefile", variable)
        if not objects:
            raise ValueError("Vendor media object list is empty: {}".format(directory))
        objects.append("aico-media-pages.o")
        if module == "rockit":
            lines.extend(["ifneq ($(CONFIG_ROCKCHIP_THUNDER_BOOT_SERVICE),y)",
                          "rockit-y += rockit/fakecode.o", "endif"])
        for name in objects:
            if not (directory / pathlib.Path(name).with_suffix(".S")).exists() and not (
                directory / pathlib.Path(name).with_suffix(".c")
            ).exists():
                raise ValueError("Missing vendor media object source: {}".format(name))
            lines.append("{}-y += {}/{}".format(module, directory.name, name))
    (output / "Makefile").write_text("\n".join(lines) + "\n")
    (output / "compatibility.json").write_text(json.dumps({
        "page_conversions": 4,
        "kernel_page_layout": "compiled from configured kernel headers",
        "sdk": str(sdk),
    }, indent=2) + "\n")
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sdk", type=pathlib.Path, required=True)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    parser.add_argument("--kernel-build", type=pathlib.Path,
                        help="Also build both modules using this configured kernel output")
    parser.add_argument("--cross-compile", default="arm-rockchip830-linux-uclibcgnueabihf-")
    parser.add_argument("--jobs", type=int, default=1)
    args = parser.parse_args()
    try:
        if args.jobs < 1:
            raise ValueError("--jobs must be positive")
        output = stage(args.sdk, args.output)
        if args.kernel_build:
            kernel_build = args.kernel_build.resolve()
            if not (kernel_build / ".config").is_file():
                raise ValueError("Configured kernel output is missing .config: {}".format(kernel_build))
            subprocess.run(["make", "-C", str(kernel_build), "M=" + str(output),
                            "ARCH=arm", "CROSS_COMPILE=" + args.cross_compile,
                            "-j" + str(args.jobs), "modules"], check=True)
        print("Prepared media compatibility modules: {}".format(output))
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print("Media compatibility preparation failed: {}".format(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
