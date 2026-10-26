---
sidebar_position: 6
---

# Wi-Fi Regulatory Domain

## Current implementation

Aico Link uses the AIC8800 driver's self-managed regulatory-domain path. The
persisted `country=` value in
`/userdata/debian/wifi/wpa_supplicant-wlan0.conf` is the authoritative user
configuration, but `wpa_supplicant` does not apply that value to the
self-managed PHY by itself.

The agent therefore applies the country code through the AIC8800 nl80211
vendor command:

1. The migration service creates the default `country=CN` configuration.
2. The Wi-Fi driver service loads the driver.
3. The agent applies the persisted country code to each Wi-Fi PHY.
4. `wpa_supplicant` starts after the PHY country code has been applied.
5. Runtime changes update the candidate configuration, apply it to the PHY,
   restart the supplicant when required, and commit the configuration only
   after the operation succeeds.

`00` is not presented as an automatic or worldwide fallback. On this driver,
`00` represents the self-managed default rules and is rejected by the country
selection API.

## Configuration and API

The main implementation is distributed across:

| Area | Location |
| --- | --- |
| Configuration loading and normalization | `src/agent/internal/configweb/wifi.go` |
| Country-code driver integration | `src/agent/internal/wifiregion/` |
| Driver and service startup | `overlay-debian/usr/lib/aico/aico-wifi-driver` and the related systemd unit |
| Configuration UI | `src/config_web/web/assets/js/config/wifi.js` |
| Persistent Wi-Fi configuration | `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` |

The region endpoints are:

| Method and endpoint | Purpose |
| --- | --- |
| `GET /api/network/wifi/region` | Return the current region, provenance, candidates, and supported options. |
| `POST /api/network/wifi/region/resolve` | Resolve candidates using `{"browser_timezone":"..."}` without recording user intent. |
| `PUT /api/network/wifi/region` | Apply an explicit choice such as `{"country":"US"}`. |

An update applies the explicit user choice to the PHY and candidate supplicant
configuration, then returns a `region` object containing the selected
`country` and its `source`, together with Wi-Fi status. The API does not
promise that the response separately returns the persisted configuration or
sidecar contents. Sidecar persistence is best-effort metadata; the
configuration file remains authoritative if the sidecar cannot be written.

The configuration file remains authoritative. If the sidecar disagrees with
the configuration file, the sidecar is discarded and regenerated from the
configuration file.

## Failure handling

Applying a new region is transactional. If driver application, supplicant
startup, or the first connection attempt fails, the service attempts to
restore the previous PHY country code and persistent configuration. Clients
must inspect the returned `wifi_rollback.ok`, `disk_error`, and `radio_error`
fields rather than assuming rollback succeeded. The temporary candidate file
is removed when possible.

A connection request must not be used to change the region implicitly. If a
connection request contains a country different from the resolved region,
the server rejects it and requires the client to call the region endpoint
first.

PHY state can take a short time to become observable after the vendor command
returns. Readers and tests should poll for up to three seconds rather than
assuming an immediate read-back.

## Validation scope

The implementation has been validated for:

- CN → US → CN changes without reboot;
- persistence across a complete reboot, with the PHY country applied before
  `wpa_supplicant` starts;
- rollback after a rejected supplicant candidate configuration;
- rollback after an initial connection failure;
- recovery when the metadata sidecar is stale; and
- read-back of PHY country and channel rules after each change.

Before release, the exposed country list must be narrowed to the product's
certified regions. The current implementation table is only the upper bound
of countries recognized by the driver. A connected-network regression must
also cover association and DHCP after a live region change.

For acceptance, check the country on each Wi-Fi PHY rather than only the
global regulatory-domain value, verify the expected channel and transmit-power
restrictions for CN and US, confirm that startup applies the country before
`wpa_supplicant`, and test association and DHCP on a connected network.
