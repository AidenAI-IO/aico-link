#!/usr/bin/env python3
import csv
import hashlib
import json
import pathlib
import sys


def spdx_id(name: str, architecture: str) -> str:
    safe = "".join(ch if ch.isalnum() or ch in ".-" else "-" for ch in name)
    return f"SPDXRef-Package-{safe}-{architecture}"


def main() -> int:
    if len(sys.argv) != 3:
        print("Usage: generate-spdx.py PACKAGES_TSV OUTPUT_JSON", file=sys.stderr)
        return 2
    package_path = pathlib.Path(sys.argv[1])
    output_path = pathlib.Path(sys.argv[2])
    package_bytes = package_path.read_bytes()
    packages = []
    relationships = []
    with package_path.open(newline="", encoding="utf-8") as stream:
        for row in csv.DictReader(stream, delimiter="\t"):
            identifier = spdx_id(row["package"], row["architecture"])
            packages.append(
                {
                    "SPDXID": identifier,
                    "name": row["package"],
                    "versionInfo": row["version"],
                    "supplier": f"Organization: {row['maintainer']}",
                    "downloadLocation": "NOASSERTION",
                    "filesAnalyzed": False,
                    "licenseConcluded": "NOASSERTION",
                    "licenseDeclared": "NOASSERTION",
                    "copyrightText": "NOASSERTION",
                    "externalRefs": [
                        {
                            "referenceCategory": "PACKAGE-MANAGER",
                            "referenceType": "purl",
                            "referenceLocator": (
                                f"pkg:deb/debian/{row['package']}@{row['version']}"
                                f"?arch={row['architecture']}"
                            ),
                        }
                    ],
                }
            )
            relationships.append(
                {
                    "spdxElementId": "SPDXRef-DOCUMENT",
                    "relationshipType": "DESCRIBES",
                    "relatedSpdxElement": identifier,
                }
            )
    digest = hashlib.sha256(package_bytes).hexdigest()
    document = {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": "aico-debian13-armhf-rootfs",
        "documentNamespace": f"https://aico.link/spdx/debian13/{digest}",
        "creationInfo": {
            "created": "2026-01-02T13:28:36Z",
            "creators": ["Tool: aico-debian-system-generate-spdx"],
            "licenseListVersion": "3.26",
        },
        "packages": packages,
        "relationships": relationships,
    }
    output_path.write_text(
        json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
