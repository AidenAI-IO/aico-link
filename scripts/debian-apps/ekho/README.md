# Ekho build inputs

The application builder compiles Ekho 9.0 from the commit, URL, and SHA-256 in
[`source.env`](source.env). The pinned upstream archive contains both the code
and original Yali Mandarin voice data. A corrupt cached archive is discarded
and the build fails before extraction; rerun to download a fresh copy.

[`english-female.patch`](english-female.patch) selects `en-us+f3` through
`espeak_SetVoiceByName()` in `EkhoImpl::initEnglish()` in `src/english.cpp`.
This applies to English and English embedded in Mandarin input. It uses the
Debian `libespeak-ng1` engine and `espeak-ng-data` voices; it does not require a
separate eSpeak executable. The Agent calls Ekho directly with
`-v Mandarin -s 20 --english-speed -10 -a 0` and sentence buffering.

From the repository root on a Linux amd64 Docker build host:

```bash
scripts/debian-apps/build-apps.sh builder
scripts/debian-apps/build-apps.sh ekho
```

The standalone `ekho` action requires neither Go nor OpenCV and leaves other
application outputs intact. Normal `all` and `apps` builds include it
automatically. `DEBIAN_APPS_EKHO_CACHE` overrides `.cache/debian-apps/ekho`.
The helper cross-compiles for Debian armhf, strips the executable, and checks
its ARM hard-float ABI and `$ORIGIN/../lib` RUNPATH.

Staged files under `output/debian-apps/apps/`:

| Build output | Installed by `aico-business` |
| --- | --- |
| `bin/ekho` | `/usr/lib/aico/ekho` |
| `share/ekho-data/` | `/usr/share/aico/ekho-data/` |
| `share/doc/ekho/` | `/usr/share/doc/aico-business/ekho/` |
| `metadata/ekho-files.sha256` | Build-time payload verification |

Only Mandarin and alphabet assets are staged. Prebuilt `zh.dict` and
`pinyin.index` avoid generating dictionary files in the read-only system data
directory. The compiled default data path is `/usr/share/aico/ekho-data`;
`EKHO_DATA_PATH` can point an isolated smoke test at staged data.

The documentation directory contains the original `COPYING` and
`COPYING-mandarin-yali` licenses, build provenance, the full upstream source
archive, the female-voice patch, and the build helper/container inputs. Source
and voice archives are downloaded during the build, not committed to this
repository. See [Debian package installation](../../../docs/08-ota/debian-package.md)
for the supported installation flow and a synthesis check.
