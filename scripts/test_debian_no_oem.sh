#!/usr/bin/env bash
set -euo pipefail
readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "${REPO_ROOT}" <<'PYTHON'
from pathlib import Path
import json, re, sys
root = Path(sys.argv[1])
def read(name): return (root/name).read_text()
board = read('scripts/debian-system/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk')
layout = re.search(r'RK_PARTITION_CMD_IN_ENV="([^"]+)"', board)[1]
parts = re.findall(r'([^,]+)\(([^)]+)\)', layout)
expected = [('32K','env'),('512K@32K','idblock'),('256K','uboot'),('4M','misc'),
            ('32M','boot_a'),('32M','boot_b'),('1792M','rootfs_a'),('1792M','rootfs_b'),
            ('3G','userdata'),('300M','ota')]
assert parts == expected, parts
slot = read('overlay-debian/usr/lib/aico/aico-slot-resolve')
for number, (_, name) in enumerate(parts, 1):
    if number >= 4:
        assert f'{name}) number={number} ;;' in slot, name
for unit, number in [('userdata.mount',9),('userdata-ota.mount',10)]:
    assert f'What=/dev/mmcblk0p{number}\n' in read('overlay-debian/etc/systemd/system/'+unit)
assert not (root/'overlay-debian-oem').exists()
assert not (root/'overlay-debian/oem').exists()
assert not (root/'overlay-debian/etc/systemd/system/oem.mount').exists()
assert read('overlay-debian/etc/locale.conf') == 'LANG=C.UTF-8\n'
contract = json.loads(read('overlay-debian/usr/lib/aico/platform/contract.json'))
assert type(contract['platform_contract']) is int and contract['platform_contract'] == 1
assert contract['architecture'] == 'armhf'
for line in read('overlay-debian/etc/systemd/system-preset/90-aico.preset').splitlines():
    line = line.strip()
    if not line or line.startswith('#'): continue
    fields = line.split()
    assert len(fields) >= 2 and fields[0] in ('enable', 'disable', 'ignore'), line
for path in (root/'overlay-debian').rglob('*'):
    if not path.is_file(): continue
    try: text = path.read_text()
    except UnicodeError: continue
    assert '/oem' not in text and 'oem.mount' not in text, path
package = read('scripts/debian-package/container-build.sh')
for name in ['frame_service_cli','audio_service_cli']:
    assert name in package
assert 'assets/business/' in package
assert 'ln -s ' not in package
assert 'config_aivqe.json' not in package
assert (root/'overlay-debian/usr/share/aico/audio/config_aivqe.json').is_file()
for path in ['assets/business/models','assets/business/audio/voice-notifications',
             'overlay-debian/usr/lib/aico/platform/lib','overlay-debian/usr/share/aico/edid']:
    assert (root/path).is_dir(), path
# rootfs creation must embed platform data before producing its archive.
rootfs = read('scripts/debian-system/container-build-rootfs.sh')
assert rootfs.index('    stage_platform\n') < rootfs.index('tar --sort=name')
assert 'chroot "${ROOTFS_DIR}" /sbin/ldconfig' in rootfs
assembly = read('scripts/debian-system/container-assemble-images.sh')
assert 'oem.img' not in assembly
assert 'rootfs-bsp-inputs.sha256' in assembly and 'ota-public-key.sha256' in assembly
print('No-OEM layout, runtime paths and package ownership checks passed')
PYTHON
