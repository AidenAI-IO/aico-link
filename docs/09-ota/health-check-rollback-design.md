---
sidebar_position: 8
---

# OTA Health Checks, Data Compatibility, and A/B Rollback Design

OTA updates write only the inactive A/B slot. Before writing, the updater
creates a durable snapshot of the protected configuration files listed in the
transaction manifest. Append-only user data remains in place.

After reboot, the OTA health aggregator runs read-only service probes. Required
probe failures prevent the health marker from being written, so the boot attempt
remains eligible for rollback. Agent startup is optional during this recovery;
the Config Web portal remains required so a broken Agent configuration can be
repaired. A successful marker commits the selected slot and clears the pending
transaction.

Rollback selects the last successful slot and restores the protected snapshot
before committing the bootloader selection. Early boot recovery reconciles
completed and abandoned rollback requests without restoring unrelated user
data. Debian production images run recovery through
`aico-ota-recovery.service` before userdata migration and use the systemd OTA
health aggregator for service checks.
